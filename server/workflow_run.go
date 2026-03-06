// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sirupsen/logrus"
)

// WorkflowRunWebhookPayload represents the workflow_run webhook payload with inputs
type WorkflowRunWebhookPayload struct {
	Action      string                 `json:"action"`
	WorkflowRun WorkflowRunWithInputs  `json:"workflow_run"`
	Repository  map[string]interface{} `json:"repository"`
	Workflow    map[string]interface{} `json:"workflow"`
}

// WorkflowRunWithInputs extends WorkflowRun with inputs field
type WorkflowRunWithInputs struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	HeadBranch string            `json:"head_branch"`
	HeadSHA    string            `json:"head_sha"`
	Inputs     map[string]string `json:"inputs"`
}

// ParseWorkflowRunEventWithInputs parses workflow_run event and extracts inputs
func ParseWorkflowRunEventWithInputs(data io.Reader) (*WorkflowRunWebhookPayload, error) {
	decoder := json.NewDecoder(data)
	var payload WorkflowRunWebhookPayload
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode workflow_run webhook payload: %w", err)
	}

	return &payload, nil
}

// handleWorkflowRunEventWithInputs handles GitHub workflow_run events.
// Routes events to CMT, nightly trigger, or test-workflow completion handlers.
func (s *Server) handleWorkflowRunEventWithInputs(payload *WorkflowRunWebhookPayload) {
	// Extract repository info
	repoData := payload.Repository
	repoName, ok := repoData["name"].(string)
	if !ok {
		s.Logger.Error("Failed to extract repository name from workflow_run payload")
		return
	}

	repoOwner, ok := repoData["owner"].(map[string]interface{})
	if !ok {
		s.Logger.Error("Failed to extract repository owner from workflow_run payload")
		return
	}
	owner, ok := repoOwner["login"].(string)
	if !ok {
		s.Logger.Error("Failed to extract owner login from workflow_run payload")
		return
	}

	workflowName := payload.WorkflowRun.Name
	headBranch := payload.WorkflowRun.HeadBranch
	headSHA := payload.WorkflowRun.HeadSHA
	runID := payload.WorkflowRun.ID

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":     repoName,
		"owner":    owner,
		"workflow": workflowName,
		"action":   payload.Action,
		"run_id":   runID,
		"head_sha": headSHA,
	})

	// --- CMT trigger workflows ---
	// "CMT Provisioner" (name contains "CMT") is dispatched by users with server_versions.
	// "Compatibility Matrix Testing" (the actual test workflow) is dispatched by Matterwick
	// with CMT_MATRIX; its completion triggers sha-based cleanup via isE2ETestWorkflow.
	if strings.Contains(workflowName, "cmt") || strings.Contains(workflowName, "CMT") {
		if payload.Action == "completed" {
			logger.Debug("CMT trigger workflow completed; sha-based cleanup is primary")
			s.handleCMTRunCleanup(repoName, headSHA, logger)
			return
		}
		if payload.Action != "requested" {
			logger.Debug("Ignoring CMT workflow action (not requested or completed)")
			return
		}
		logger.Info("Processing CMT workflow_run event")
		serverVersionsStr, ok := payload.WorkflowRun.Inputs["server_versions"]
		if !ok || serverVersionsStr == "" {
			logger.Error("No server_versions found in workflow inputs")
			return
		}
		serverVersions := parseServerVersionsFromString(serverVersionsStr)
		if len(serverVersions) == 0 {
			logger.Error("Failed to parse server versions from workflow input")
			return
		}
		logger.WithField("serverVersions", serverVersions).Info("Extracted server versions from workflow inputs")
		var instanceType string
		if strings.Contains(repoName, "desktop") {
			instanceType = "desktop"
		} else if strings.Contains(repoName, "mobile") {
			instanceType = "mobile"
		} else {
			logger.Warn("Repository is neither desktop nor mobile, skipping CMT")
			return
		}
		go s.handleCMTWithServerVersions(owner, repoName, instanceType, headBranch, headSHA, serverVersions, runID, logger)
		return
	}

	// --- Nightly trigger workflow ---
	// When the lightweight nightly trigger workflow is requested, provision instances and
	// dispatch the actual test workflow. The nightly trigger workflow does no testing itself.
	if s.Config.E2ENightlyTriggerWorkflowName != "" && workflowName == s.Config.E2ENightlyTriggerWorkflowName {
		if payload.Action == "requested" {
			logger.Info("Nightly trigger workflow started, provisioning E2E servers")
			go s.handleNightlyE2ETrigger(owner, repoName, headBranch, headSHA, logger)
		}
		return
	}

	// --- Test workflow completion: sha-based cleanup for push events and nightly runs ---
	if payload.Action == "completed" && s.isE2ETestWorkflow(workflowName) {
		logger.Info("Test workflow completed, checking for sha-based instance cleanup")
		s.findAndDestroyInstancesBySHA(repoName, headSHA, logger)
		return
	}

	logger.Debug("Ignoring workflow_run event (not relevant to E2E lifecycle)")
}

// handleNightlyE2ETrigger provisions instances and dispatches the test workflow
// for scheduled/nightly E2E runs. Called when the nightly trigger workflow starts.
func (s *Server) handleNightlyE2ETrigger(owner, repoName, branch, sha string, logger logrus.FieldLogger) {
	logger = logger.WithFields(logrus.Fields{
		"branch": branch,
		"sha":    sha,
	})
	logger.Info("Provisioning nightly E2E instances")

	instanceType := "desktop"
	if strings.Contains(repoName, "mobile") {
		instanceType = "mobile"
	} else if !strings.Contains(repoName, "desktop") {
		logger.Warn("Repository is neither desktop nor mobile, skipping nightly E2E trigger")
		return
	}

	instances, err := s.createCMTInstancesForVersion(repoName, instanceType, s.Config.E2EServerVersion, "nightly")
	if err != nil {
		logger.WithError(err).Error("Failed to create nightly E2E instances")
		return
	}

	// Track by sha so the test workflow completion can clean up
	key := fmt.Sprintf("%s-scheduled-%s", repoName, sha)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.WithField("tracking_key", key).Info("Nightly instances tracked, dispatching test workflow")

	var dispatchErr error
	if instanceType == "desktop" {
		instanceDetailsJSON, err := s.buildInstanceDetailsJSON(instances)
		if err != nil {
			logger.WithError(err).Error("Failed to build instance details JSON for nightly desktop run")
			s.e2eInstancesLock.Lock()
			delete(s.e2eInstances, key)
			s.e2eInstancesLock.Unlock()
			s.destroyE2EInstances(instances, logger)
			return
		}
		// Dispatch to the exact SHA so workflow_run completed event matches the tracking key.
		// Pass runType="NIGHTLY" and nightly=true so the workflow correctly classifies this run.
		dispatchErr = s.dispatchDesktopE2EWorkflow(owner, repoName, sha, sha, instanceDetailsJSON, "NIGHTLY", true)
	} else {
		if len(instances) < 3 {
			logger.Errorf("Expected 3 mobile instances, got %d", len(instances))
			s.e2eInstancesLock.Lock()
			delete(s.e2eInstances, key)
			s.e2eInstancesLock.Unlock()
			s.destroyE2EInstances(instances, logger)
			return
		}
		// Dispatch to the exact SHA so workflow_run completed event matches the tracking key.
		// Pass runType="NIGHTLY" so the workflow correctly classifies this as a nightly run.
		dispatchErr = s.dispatchMobileE2EWorkflow(owner, repoName, sha, sha,
			instances[0].URL, instances[1].URL, instances[2].URL, "both", "NIGHTLY")
	}

	if dispatchErr != nil {
		logger.WithError(dispatchErr).Error("Failed to dispatch test workflow for nightly run; cleaning up instances")
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.Info("Nightly E2E workflow dispatched successfully")
}

// isE2ETestWorkflow returns true if the workflow name is a configured E2E test workflow
// (as opposed to a trigger or CMT provisioner workflow).
func (s *Server) isE2ETestWorkflow(name string) bool {
	for _, n := range s.Config.E2ETestWorkflowNames {
		if n == name {
			return true
		}
	}
	return false
}

// findAndDestroyInstancesBySHA scans the instance map for entries belonging to repoName
// whose tracking key ends with "-{headSHA}" (push-event, scheduled, and cmt keys) and destroys them.
func (s *Server) findAndDestroyInstancesBySHA(repoName, headSHA string, logger logrus.FieldLogger) {
	if headSHA == "" {
		return
	}
	prefix := repoName + "-"
	suffix := "-" + headSHA

	s.e2eInstancesLock.Lock()
	var found []*E2EInstance
	var keysToDelete []string
	for key, instances := range s.e2eInstances {
		if strings.HasPrefix(key, prefix) && strings.HasSuffix(key, suffix) {
			found = append(found, instances...)
			keysToDelete = append(keysToDelete, key)
		}
	}
	for _, k := range keysToDelete {
		delete(s.e2eInstances, k)
	}
	s.e2eInstancesLock.Unlock()

	if len(found) == 0 {
		logger.Debug("No sha-tracked instances found for cleanup")
		return
	}
	logger.WithField("instances", len(found)).Info("Destroying sha-tracked instances for completed workflow")
	s.destroyE2EInstances(found, logger)
}

// parseServerVersionsFromString parses comma-separated server versions string
// Example input: "v11.1.0, v11.2.0, v12.0.0"
// Returns: ["v11.1.0", "v11.2.0", "v12.0.0"]
func parseServerVersionsFromString(input string) []string {
	versions := splitCommaSeparated(input)
	if versions == nil {
		return []string{}
	}
	return versions
}

// handleCMTWithServerVersions orchestrates CMT testing: creates one instance per server
// version, builds the CMT_MATRIX JSON, and dispatches compatibility-matrix-testing.yml once.
func (s *Server) handleCMTWithServerVersions(repoOwner, repoName, instanceType, branch, sha string, serverVersions []string, runID int64, logger logrus.FieldLogger) {
	// Cap at 5 versions to prevent runaway provisioning
	const maxVersions = 5
	if len(serverVersions) > maxVersions {
		logger.Warnf("Capping server versions from %d to %d", len(serverVersions), maxVersions)
		serverVersions = serverVersions[:maxVersions]
	}

	logger = logger.WithFields(logrus.Fields{
		"serverVersionCount": len(serverVersions),
		"branch":             branch,
		"sha":                sha,
		"run_id":             runID,
	})
	logger.Info("Starting CMT with server versions")

	// Create one instance per version. The CMT matrix cross-products environment × server,
	// so a single server URL handles all platform test runners for that version.
	var allInstances []*E2EInstance
	var validVersions []string

	for _, version := range serverVersions {
		version = strings.TrimSpace(version)
		if version == "" {
			continue
		}

		logger.WithField("version", version).Info("Creating CMT instance for server version")

		instance, err := s.createSingleCMTInstance(repoName, instanceType, version, logger)
		if err != nil {
			logger.WithError(err).Errorf("Failed to create instance for version %s, skipping", version)
			continue
		}

		allInstances = append(allInstances, instance)
		validVersions = append(validVersions, version)
	}

	if len(allInstances) == 0 {
		logger.Error("No instances created for any version")
		return
	}

	logger.WithField("totalInstances", len(allInstances)).Info("CMT instances created, tracking for cleanup")

	// Track by runID+sha: runID prevents collision when two dispatches share the same
	// branch HEAD SHA; the key still ends with "-{sha}" so findAndDestroyInstancesBySHA
	// can locate it when compatibility-matrix-testing.yml completes (hours later).
	key := fmt.Sprintf("%s-cmt-%d-%s", repoName, runID, sha)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = allInstances
	s.e2eInstancesLock.Unlock()

	// Build CMT_MATRIX JSON and dispatch compatibility-matrix-testing.yml.
	var cmtMatrixJSON string
	var buildErr error
	if instanceType == "desktop" {
		cmtMatrixJSON, buildErr = buildDesktopCMTMatrixJSON(validVersions, allInstances)
	} else {
		cmtMatrixJSON, buildErr = buildMobileCMTMatrixJSON(validVersions, allInstances)
	}
	if buildErr != nil {
		logger.WithError(buildErr).Error("Failed to build CMT_MATRIX JSON")
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(allInstances, logger)
		return
	}

	// Dispatch to the exact SHA so the completed workflow_run event carries the same
	// head_sha, allowing findAndDestroyInstancesBySHA to match the tracking key.
	if err := s.dispatchCMTWorkflow(repoOwner, repoName, sha, branch, cmtMatrixJSON, instanceType, logger); err != nil {
		logger.WithError(err).Error("Failed to dispatch compatibility-matrix-testing.yml")
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(allInstances, logger)
		return
	}

	logger.WithField("tracking_key", key).Info("CMT workflow dispatched successfully; instances tracked for cleanup")
}

// createSingleCMTInstance creates one Mattermost cloud instance for a CMT server version.
// Unlike createCMTInstancesForVersion (which creates 3 platform-specific instances for
// nightly runs), CMT only needs one server — the matrix handles parallelism.
func (s *Server) createSingleCMTInstance(repoName, instanceType, version string, logger logrus.FieldLogger) (*E2EInstance, error) {
	sanitizedRepoName := strings.ToLower(repoName)
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, "_", "-")
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, ".", "-")

	sanitizedVersion := strings.ToLower(version)
	sanitizedVersion = strings.ReplaceAll(sanitizedVersion, ".", "-")

	suffix := fmt.Sprintf("-cmt-%s", sanitizedVersion)
	repoPrefix := sanitizedRepoName
	if maxLen := 63 - len(s.Config.DNSNameTestServer) - len(suffix); len(repoPrefix) > maxLen {
		if maxLen < 1 {
			maxLen = 1
		}
		repoPrefix = strings.TrimRight(repoPrefix[:maxLen], "-")
	}
	name := repoPrefix + suffix

	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)

	return s.createCloudInstallation(name, version, username, password, instanceType, logger)
}

// cmtServer is the server entry in CMT_MATRIX JSON.
type cmtServer struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// buildDesktopCMTMatrixJSON builds the CMT_MATRIX JSON for compatibility-matrix-testing.yml
// in the desktop repo. The matrix cross-products environment × server, so one server URL
// is shared across all three platform runners.
//
// Schema:
//
//	{
//	  "environment": [
//	    {"os": "linux", "runner": "ubuntu-22.04"},
//	    {"os": "macos", "runner": "macos-13"},
//	    {"os": "windows", "runner": "windows-2022"}
//	  ],
//	  "server": [
//	    {"version": "v11.1.0", "url": "https://..."},
//	    ...
//	  ]
//	}
func buildDesktopCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type cmtEnvironment struct {
		OS     string `json:"os"`
		Runner string `json:"runner"`
	}
	type desktopCMTMatrix struct {
		Environment []cmtEnvironment `json:"environment"`
		Server      []cmtServer      `json:"server"`
	}

	matrix := desktopCMTMatrix{
		Environment: []cmtEnvironment{
			{OS: "linux", Runner: "ubuntu-22.04"},
			{OS: "macos", Runner: "macos-13"},
			{OS: "windows", Runner: "windows-2022"},
		},
	}
	for i, version := range versions {
		if i >= len(instances) {
			break
		}
		matrix.Server = append(matrix.Server, cmtServer{Version: version, URL: instances[i].URL})
	}

	b, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("failed to marshal desktop CMT matrix: %w", err)
	}
	return string(b), nil
}

// buildMobileCMTMatrixJSON builds the CMT_MATRIX JSON for compatibility-matrix-testing.yml
// in the mobile repo. One iOS test job is created per server version.
//
// Schema:
//
//	{
//	  "server": [
//	    {"version": "v11.1.0", "url": "https://..."},
//	    ...
//	  ]
//	}
func buildMobileCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type mobileCMTMatrix struct {
		Server []cmtServer `json:"server"`
	}

	var matrix mobileCMTMatrix
	for i, version := range versions {
		if i >= len(instances) {
			break
		}
		matrix.Server = append(matrix.Server, cmtServer{Version: version, URL: instances[i].URL})
	}

	b, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("failed to marshal mobile CMT matrix: %w", err)
	}
	return string(b), nil
}

// dispatchCMTWorkflow dispatches compatibility-matrix-testing.yml with the populated
// CMT_MATRIX JSON. Dispatches with ref=sha so the resulting workflow_run.head_sha matches
// the tracking key suffix, enabling sha-based cleanup on completion.
func (s *Server) dispatchCMTWorkflow(repoOwner, repoName, sha, branch, cmtMatrixJSON, instanceType string, logger logrus.FieldLogger) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	workflowInputs := map[string]interface{}{
		"CMT_MATRIX": cmtMatrixJSON,
	}
	if instanceType == "desktop" {
		workflowInputs["DESKTOP_VERSION"] = branch
	} else {
		workflowInputs["MOBILE_VERSION"] = branch
	}

	logger.WithFields(logrus.Fields{
		"ref":          sha,
		"instanceType": instanceType,
	}).Debug("Dispatching compatibility-matrix-testing.yml")

	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/compatibility-matrix-testing.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    sha,
			"inputs": workflowInputs,
		})
	if err != nil {
		return fmt.Errorf("failed to create CMT workflow dispatch request: %w", err)
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("failed to dispatch compatibility-matrix-testing.yml: %w", err)
	}
	if resp.StatusCode != 204 {
		return fmt.Errorf("unexpected status %d from compatibility-matrix-testing.yml dispatch", resp.StatusCode)
	}

	logger.Info("compatibility-matrix-testing.yml dispatched successfully")
	return nil
}

// createCMTInstancesForVersion creates 3 instances (one per platform) for a given server
// version. Used by nightly runs which dispatch the platform-aware e2e-functional.yml /
// e2e-detox-pr.yml workflows (not the CMT matrix workflow).
func (s *Server) createCMTInstancesForVersion(repoName, instanceType, version, purpose string) ([]*E2EInstance, error) {
	var instances []*E2EInstance
	var platforms []string

	if instanceType == "desktop" {
		platforms = []string{"linux", "macos", "windows"}
	} else {
		platforms = []string{"site-1", "site-2", "site-3"}
	}

	sanitizedRepoName := strings.ToLower(repoName)
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, "_", "-")
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, ".", "-")

	sanitizedVersion := strings.ReplaceAll(version, ".", "-")

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":    repoName,
		"type":    instanceType,
		"version": version,
	})

	// Get credentials
	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)

	for _, platform := range platforms {
		suffix := fmt.Sprintf("-%s-%s-%s", purpose, sanitizedVersion, platform)
		repoPrefix := sanitizedRepoName
		if maxLen := 63 - len(s.Config.DNSNameTestServer) - len(suffix); len(repoPrefix) > maxLen {
			if maxLen < 1 {
				maxLen = 1
			}
			repoPrefix = strings.TrimRight(repoPrefix[:maxLen], "-")
		}
		name := repoPrefix + suffix

		instance, err := s.createCloudInstallation(name, version, username, password, instanceType, logger)
		if err != nil {
			logger.WithError(err).Errorf("Failed to create instance for platform %s", platform)
			// Cleanup already created instances on failure
			s.destroyE2EInstances(instances, logger)
			return nil, err
		}

		instance.Platform = platform
		if instanceType == "desktop" {
			instance.Runner = getRunnerForPlatform(platform)
		}
		instances = append(instances, instance)
	}

	logger.WithField("instanceCount", len(instances)).Info("Instances created for version")
	return instances, nil
}

// handleCMTRunCleanup is a best-effort fallback for CMT cleanup when the trigger workflow
// completes. Because the CMT trigger is a lightweight workflow that completes in seconds —
// well before the 30-minute provisioning goroutine stores instances — this function will
// most often find nothing. The primary cleanup path is findAndDestroyInstancesBySHA,
// triggered when compatibility-matrix-testing.yml completes.
func (s *Server) handleCMTRunCleanup(repoName, sha string, logger logrus.FieldLogger) {
	logger = logger.WithFields(logrus.Fields{
		"repo": repoName,
		"sha":  sha,
		"type": "cmt_cleanup_fallback",
	})
	logger.Debug("CMT trigger completed — sha-based cleanup is the primary path")
	s.findAndDestroyInstancesBySHA(repoName, sha, logger)
}

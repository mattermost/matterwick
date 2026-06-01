// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

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
	Event      string            `json:"event"` // triggering event: "push", "schedule", "workflow_dispatch", etc.
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

// handleWorkflowRunEventWithInputs routes workflow_run events to CMT, nightly, or cleanup handlers.
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

	// CMT: a lightweight, scheduled trigger workflow (Config.CMTTriggerWorkflowName, e.g.
	// "CMT Provisioner") fires in the desktop/mobile repo. matterwick provisions one instance
	// per version in Config.CMTVersions() and dispatches compatibility-matrix-testing.yml.
	// No inputs are read from the event — the version set is hardcoded in matterwick config —
	// so this works despite GitHub's workflow_run payload not carrying workflow_dispatch inputs.
	// Cleanup happens when the test workflow ("Compatibility Matrix Testing") completes, handled
	// by the isE2ETestWorkflow branch below.
	if s.Config.CMTTriggerWorkflowName != "" && workflowName == s.Config.CMTTriggerWorkflowName {
		if payload.Action == "requested" {
			logger.Info("CMT trigger workflow started, provisioning E2E servers for configured versions")
			go s.handleCMTTrigger(owner, repoName, headBranch, headSHA, runID, logger)
		}
		return
	}

	// Nightly: lightweight trigger workflow fires first; matterwick provisions instances and dispatches the real test workflow.
	if s.Config.E2ENightlyTriggerWorkflowName != "" && workflowName == s.Config.E2ENightlyTriggerWorkflowName {
		if payload.Action == "requested" {
			logger.Info("Nightly trigger workflow started, provisioning E2E servers")
			go s.handleNightlyE2ETrigger(owner, repoName, headBranch, headSHA, payload.WorkflowRun.Event, runID, logger)
		}
		return
	}

	// --- Test workflow completion: clean up provisioned instances ---
	//
	// Cleanup is keyed by head_sha against tracking-key suffixes ({repo}-...-{sha}).
	// GitHub's workflow_run webhook payload does not include workflow_dispatch inputs,
	// so SHA-based scanning is the canonical mechanism for matching provisioned instances
	// (push, scheduled, and CMT tracking keys all end with "-{sha}").
	if payload.Action == "completed" && s.isE2ETestWorkflow(workflowName) {
		logger.Info("Test workflow completed, cleaning up instances by SHA suffix match")
		s.findAndDestroyInstancesBySHA(repoName, headSHA, logger)
		return
	}

	logger.WithFields(logrus.Fields{
		"configured_nightly_name":   s.Config.E2ENightlyTriggerWorkflowName,
		"configured_test_workflows": s.Config.E2ETestWorkflowNames,
	}).Info("Ignoring workflow_run event (not relevant to E2E lifecycle)")
}

// handleNightlyE2ETrigger provisions instances and dispatches the test workflow.
// Called when the E2E trigger workflow starts, whether from schedule, push to master/main,
// or push to a release branch. The triggerEvent parameter ("schedule", "push", etc.) is
// used to set runType correctly — scheduled runs always get "NIGHTLY" regardless of branch.
func (s *Server) handleNightlyE2ETrigger(owner, repoName, branch, sha, triggerEvent string, runID int64, logger logrus.FieldLogger) {
	logger = logger.WithFields(logrus.Fields{
		"branch": branch,
		"sha":    sha,
		"run_id": runID,
	})
	logger.Info("Provisioning nightly E2E instances")

	instanceType := "desktop"
	if strings.Contains(repoName, "mobile") {
		instanceType = "mobile"
	} else if !strings.Contains(repoName, "desktop") {
		logger.Warn("Repository is neither desktop nor mobile, skipping nightly E2E trigger")
		return
	}

	instances, err := s.createCMTInstancesForVersion(repoName, instanceType, s.resolveE2EServerVersion(), "nightly")
	if err != nil {
		logger.WithError(err).Error("Failed to create nightly E2E instances")
		return
	}

	// Include runID so two trigger runs against the same SHA (e.g. manual re-trigger)
	// get separate tracking keys. The key still ends with "-{sha}" so
	// findAndDestroyInstancesBySHA continues to match it by suffix.
	key := fmt.Sprintf("%s-scheduled-%d-%s", repoName, runID, sha)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.WithField("tracking_key", key).Info("Nightly instances tracked, dispatching test workflow")

	// Determine run classification. Scheduled runs are always NIGHTLY regardless of branch
	// (a scheduled run on master must not be classified as MASTER). Push-triggered runs
	// derive their type from the branch name.
	runType := "NIGHTLY"
	nightly := true
	if triggerEvent != "schedule" {
		if branch == "master" || branch == "main" {
			runType = "MASTER"
			nightly = false
		} else if s.isReleaseBranch(branch) {
			runType = "RELEASE"
			nightly = false
		}
	}

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
		dispatchErr = s.dispatchDesktopE2EWorkflow(owner, repoName, branch, sha, instanceDetailsJSON, runType, nightly)
	} else {
		if len(instances) < 3 {
			logger.Errorf("Expected 3 mobile instances, got %d", len(instances))
			s.e2eInstancesLock.Lock()
			delete(s.e2eInstances, key)
			s.e2eInstancesLock.Unlock()
			s.destroyE2EInstances(instances, logger)
			return
		}
		dispatchErr = s.dispatchMobileE2EWorkflow(owner, repoName, branch, sha,
			instances[0].URL, instances[1].URL, instances[2].URL, "both", runType)
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

// handleCMTTrigger is invoked when the scheduled CMT trigger workflow fires. It resolves the
// instance type from the repo, reads the hardcoded server-version set from config, and hands
// off to handleCMTWithServerVersions. The version list lives in matterwick (Config.CMTVersions),
// so nothing needs to be read from the workflow_run event.
func (s *Server) handleCMTTrigger(owner, repoName, branch, sha string, runID int64, logger logrus.FieldLogger) {
	instanceType := "desktop"
	if strings.Contains(repoName, "mobile") {
		instanceType = "mobile"
	} else if !strings.Contains(repoName, "desktop") {
		logger.Warn("Repository is neither desktop nor mobile, skipping CMT trigger")
		return
	}

	versions := s.Config.CMTVersions()
	logger.WithFields(logrus.Fields{
		"instanceType": instanceType,
		"versions":     versions,
	}).Info("Provisioning CMT instances for configured server versions")

	s.handleCMTWithServerVersions(owner, repoName, instanceType, branch, sha, versions, runID, logger)
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
		// Docker Hub tags use bare semver (e.g. "11.6.0"), not "v11.6.0".
		version = strings.TrimPrefix(version, "v")

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

	if err := s.dispatchCMTWorkflow(repoOwner, repoName, sha, branch, cmtMatrixJSON, instanceType, runID, logger); err != nil {
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
	// Name format: {type}-{version}-{hex6}
	sanitizedVersion := sanitizeForDNS(version)
	uid := e2eUniqueSuffix()
	name := e2eInstanceName(s.Config.DNSNameTestServer, instanceType, sanitizedVersion, uid)

	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)

	return s.createCloudInstallation(context.Background(), name, version, username, password, instanceType, logger)
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
// CMT_MATRIX JSON. runID is the CMT trigger workflow run ID, passed as cmt_run_id purely
// for traceability/logging on the test workflow side.
//
// We intentionally do NOT pass a "mw_tracking_key" input: compatibility-matrix-testing.yml
// does not declare it, and GitHub rejects a workflow_dispatch carrying an undeclared input
// with a 422. Cleanup is driven by SHA-suffix matching when the test workflow completes.
func (s *Server) dispatchCMTWorkflow(repoOwner, repoName, sha, branch, cmtMatrixJSON, instanceType string, runID int64, logger logrus.FieldLogger) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	workflowInputs := map[string]interface{}{
		"CMT_MATRIX": cmtMatrixJSON,
		"cmt_run_id": fmt.Sprintf("%d", runID),
	}
	if instanceType == "desktop" {
		workflowInputs["DESKTOP_VERSION"] = branch
	} else {
		workflowInputs["MOBILE_VERSION"] = branch
	}

	logger.WithFields(logrus.Fields{
		"ref":          branch,
		"instanceType": instanceType,
	}).Debug("Dispatching compatibility-matrix-testing.yml")

	// GitHub workflow_dispatch requires a branch or tag name as ref, not a commit SHA.
	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/compatibility-matrix-testing.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    branch,
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

// createCMTInstancesForVersion creates 3 instances (one per platform) in parallel for a
// given server version. Used by nightly runs which dispatch the platform-aware
// e2e-functional.yml / e2e-detox-pr.yml workflows (not the CMT matrix workflow).
// Results are returned in platforms[] order so index-based assignment is stable.
func (s *Server) createCMTInstancesForVersion(repoName, instanceType, version, purpose string) ([]*E2EInstance, error) {
	var platforms []string
	if instanceType == "desktop" {
		platforms = []string{"linux", "macos", "windows"}
	} else {
		platforms = []string{"site-1", "site-2", "site-3"}
	}

	// Name format: {type}-{version}-{platform}-{hex6}
	sanitizedVersion := sanitizeForDNS(version)
	uid := e2eUniqueSuffix()

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":    repoName,
		"type":    instanceType,
		"version": version,
	})

	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		instance *E2EInstance
		err      error
	}
	results := make([]result, len(platforms))
	var wg sync.WaitGroup

	for i, platform := range platforms {
		wg.Add(1)
		go func(idx int, platform string) {
			defer wg.Done()
			name := e2eInstanceName(
				s.Config.DNSNameTestServer,
				instanceType, sanitizedVersion, platform, uid,
			)
			inst, err := s.createCloudInstallation(ctx, name, version, username, password, instanceType, logger)
			if err != nil {
				cancel()
				results[idx] = result{err: err}
				return
			}
			inst.Platform = platform
			if instanceType == "desktop" {
				inst.Runner = getRunnerForPlatform(platform)
			}
			results[idx] = result{instance: inst}
		}(i, platform)
	}

	wg.Wait()

	var instances []*E2EInstance
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
		} else {
			instances = append(instances, r.instance)
		}
	}

	if firstErr != nil {
		logger.WithError(firstErr).Error("Failed to create one or more instances; destroying all")
		s.destroyE2EInstances(instances, logger)
		return nil, firstErr
	}

	logger.WithField("instanceCount", len(instances)).Info("Instances created for version")
	return instances, nil
}

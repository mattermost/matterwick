// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// WorkflowRunWebhookPayload is the parsed body of a workflow_run webhook event.
type WorkflowRunWebhookPayload struct {
	Action      string                 `json:"action"`
	WorkflowRun WorkflowRunWithInputs  `json:"workflow_run"`
	Repository  map[string]interface{} `json:"repository"`
	Workflow    map[string]interface{} `json:"workflow"`
}

// WorkflowRunWithInputs is the workflow_run object extended with the workflow_dispatch inputs field.
type WorkflowRunWithInputs struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	HeadBranch string            `json:"head_branch"`
	HeadSHA    string            `json:"head_sha"`
	Event      string            `json:"event"` // triggering event: "push", "schedule", "workflow_dispatch", etc.
	Inputs     map[string]string `json:"inputs"`
}

// ParseWorkflowRunEventWithInputs decodes a workflow_run webhook payload from r.
func ParseWorkflowRunEventWithInputs(data io.Reader) (*WorkflowRunWebhookPayload, error) {
	decoder := json.NewDecoder(data)
	var payload WorkflowRunWebhookPayload
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode workflow_run webhook payload: %w", err)
	}

	return &payload, nil
}

// handleWorkflowRunEventWithInputs routes workflow_run events to CMT or cleanup handlers.
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

	// CMT trigger: provision one server per version in s.cmtServerVersions() and dispatch compatibility-matrix-testing.yml.
	if s.Config.CMTTriggerWorkflowName != "" && workflowName == s.Config.CMTTriggerWorkflowName {
		if payload.Action == "requested" {
			triggerEvent := payload.WorkflowRun.Event
			if s.shouldTriggerCMT(triggerEvent, headBranch) {
				logger.WithFields(logrus.Fields{
					"trigger_event": triggerEvent,
					"head_branch":   headBranch,
				}).Info("CMT trigger workflow started, provisioning E2E servers for configured versions")
				go s.handleCMTTrigger(owner, repoName, headBranch, headSHA, runID, logger)
			} else {
				logger.WithFields(logrus.Fields{
					"trigger_event": triggerEvent,
					"head_branch":   headBranch,
				}).Info("CMT trigger fired on non-RC-tag, non-release ref and not via manual dispatch; skipping")
			}
		}
		return
	}

	// On completion: CMT keys on run id, non-CMT flows key on SHA.
	if payload.Action == "completed" && s.isE2ETestWorkflow(workflowName) {
		if workflowName == s.cmtTestWorkflowName() {
			logger.Info("CMT test workflow completed, cleaning up instances by run id")
			s.findAndDestroyInstancesByRunID(repoName, runID, logger)
		} else {
			logger.Info("Test workflow completed, cleaning up matching instances by SHA")
			s.findAndDestroyInstancesBySHA(repoName, headSHA, false, logger)
		}
		return
	}

	logger.WithField("configured_test_workflows", s.Config.E2ETestWorkflowNames).
		Info("Ignoring workflow_run event (not relevant to E2E lifecycle)")
}

// isE2ETestWorkflow reports whether name is in Config.E2ETestWorkflowNames.
func (s *Server) isE2ETestWorkflow(name string) bool {
	for _, n := range s.Config.E2ETestWorkflowNames {
		if n == name {
			return true
		}
	}
	return false
}

// defaultCMTTestWorkflowName is the "name:" of compatibility-matrix-testing.yml in the
// desktop/mobile repos. Used when Config.CMTTestWorkflowName is empty so CMT cleanup (keyed
// by run id) never silently falls back to SHA cleanup, which can't match a -cmt-{runID} key.
const defaultCMTTestWorkflowName = "Compatibility Matrix Testing"

// cmtTestWorkflowName returns the configured CMT test workflow name, or the default.
func (s *Server) cmtTestWorkflowName() string {
	if s.Config.CMTTestWorkflowName != "" {
		return s.Config.CMTTestWorkflowName
	}
	return defaultCMTTestWorkflowName
}

func cmtInstanceKey(repoName string, testRunID int64) string {
	return fmt.Sprintf("%s-cmt-%d", repoName, testRunID)
}

// cmtDispatchMutex returns a per-repo mutex for the dispatch+poll+store critical section.
func (s *Server) cmtDispatchMutex(repoName string) *sync.Mutex {
	s.cmtDispatchLocksMu.Lock()
	defer s.cmtDispatchLocksMu.Unlock()
	if m, ok := s.cmtDispatchLocks[repoName]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.cmtDispatchLocks[repoName] = m
	return m
}

// instanceKeyMatchesRunID reports whether key is the CMT tracking entry for testRunID.
func instanceKeyMatchesRunID(key, repoName string, testRunID int64) bool {
	return key == cmtInstanceKey(repoName, testRunID)
}

// claimedCMTRunIDs returns tracked CMT run ids for repoName; the poll uses this to skip already-claimed dispatches.
func (s *Server) claimedCMTRunIDs(repoName string) map[int64]bool {
	prefix := repoName + "-cmt-"
	s.e2eInstancesLock.Lock()
	defer s.e2eInstancesLock.Unlock()
	claimed := make(map[int64]bool, len(s.e2eInstances))
	for k := range s.e2eInstances {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(k[len(prefix):], "%d", &id); err == nil && id > 0 {
			claimed[id] = true
		}
	}
	return claimed
}

// removeCMTInstancesByRunID removes and returns CMT instances for testRunID. Returns nil for runID 0 (unresolved dispatch sentinel).
func (s *Server) removeCMTInstancesByRunID(repoName string, testRunID int64, logger logrus.FieldLogger) []*E2EInstance {
	if testRunID == 0 {
		return nil
	}

	key := cmtInstanceKey(repoName, testRunID)
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) == 0 {
		logger.WithField("tracking_key", key).Debug("No run-id-tracked CMT instances found for cleanup")
		return nil
	}
	logger.WithFields(logrus.Fields{
		"tracking_key": key,
		"instances":    len(instances),
	}).Info("Removed run-id-tracked CMT instances; destroying")
	return instances
}

// findAndDestroyInstancesByRunID destroys the CMT instance set keyed to the completing
// compatibility-matrix-testing.yml run id.
func (s *Server) findAndDestroyInstancesByRunID(repoName string, testRunID int64, logger logrus.FieldLogger) {
	instances := s.removeCMTInstancesByRunID(repoName, testRunID, logger)
	if len(instances) == 0 {
		return
	}
	s.destroyE2EInstances(instances, logger)
}

// instanceKeyMatchesSHA reports whether key belongs to repoName, ends with headSHA, and matches the flow type (CMT vs push/scheduled) to prevent cross-flow SHA collisions.
func instanceKeyMatchesSHA(key, repoName, headSHA string, cmtOnly bool) bool {
	if !strings.HasPrefix(key, repoName+"-") || !strings.HasSuffix(key, "-"+headSHA) {
		return false
	}
	return strings.HasPrefix(key, repoName+"-cmt-") == cmtOnly
}

// findAndDestroyInstancesBySHA destroys instances whose key ends with headSHA, scoped to CMT or non-CMT flows to prevent cross-flow teardown.
func (s *Server) findAndDestroyInstancesBySHA(repoName, headSHA string, cmtOnly bool, logger logrus.FieldLogger) {
	if headSHA == "" {
		return
	}

	s.e2eInstancesLock.Lock()
	var found []*E2EInstance
	var keysToDelete []string
	for key, instances := range s.e2eInstances {
		if !instanceKeyMatchesSHA(key, repoName, headSHA, cmtOnly) {
			continue
		}
		found = append(found, instances...)
		keysToDelete = append(keysToDelete, key)
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

// parseServerVersionsFromString splits a comma-separated version string and trims whitespace.
func parseServerVersionsFromString(input string) []string {
	versions := splitCommaSeparated(input)
	if versions == nil {
		return []string{}
	}
	return versions
}

// shouldTriggerCMT returns true for manual dispatch, RC tags, mobile build-release branches, or release branches.
func (s *Server) shouldTriggerCMT(triggerEvent, headBranch string) bool {
	return triggerEvent == "workflow_dispatch" ||
		isRCTag(headBranch) ||
		isBuildReleaseBranch(headBranch) ||
		s.isReleaseBranch(headBranch)
}

// rcTagPattern matches RC tags: optional "v", then semver, then "-rc" + number (e.g. v6.2.0-rc.1, 6.2.0-rc1).
var rcTagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+-rc[.\-]?\d+$`)

func isRCTag(ref string) bool {
	return rcTagPattern.MatchString(ref)
}

// buildReleaseBranchPattern matches mobile's build-release-NNN branches (exactly 3–4 digits). Update in sync with cmt-provisioner.yml if the convention changes.
var buildReleaseBranchPattern = regexp.MustCompile(`^build-release-\d{3,4}$`)

// isBuildReleaseBranch reports whether ref is mobile's RC-cut branch (build-release-NNN). Used as a CMT gate separate from isReleaseBranch to avoid triggering on every cherry-pick.
func isBuildReleaseBranch(ref string) bool {
	return buildReleaseBranchPattern.MatchString(ref)
}

// handleCMTTrigger resolves instance type and server versions, then delegates to handleCMTWithServerVersions.
func (s *Server) handleCMTTrigger(owner, repoName, branch, sha string, runID int64, logger logrus.FieldLogger) {
	instanceType := "desktop"
	if strings.Contains(repoName, "mobile") {
		instanceType = "mobile"
	} else if !strings.Contains(repoName, "desktop") {
		logger.Warn("Repository is neither desktop nor mobile, skipping CMT trigger")
		return
	}

	versions := s.cmtServerVersions()
	logger.WithFields(logrus.Fields{
		"instanceType": instanceType,
		"versions":     versions,
	}).Info("Provisioning CMT instances for resolved server versions")

	s.handleCMTWithServerVersions(owner, repoName, instanceType, branch, sha, versions, runID, logger)
}

// capCMTServerVersions keeps at most maxCMTServerVersions entries, preferring the
// newest parseable semvers. Copies the input so Config.CMTServerVersions is not mutated.
func capCMTServerVersions(serverVersions []string) []string {
	if len(serverVersions) <= maxCMTServerVersions {
		return serverVersions
	}
	sorted := append([]string(nil), serverVersions...)
	sort.Slice(sorted, func(i, j int) bool {
		vi, oki := parseCMTVersion(sorted[i])
		vj, okj := parseCMTVersion(sorted[j])
		if oki != okj {
			return !oki // unparseable first so the newest tail keeps valid versions
		}
		if !oki {
			return sorted[i] < sorted[j]
		}
		return vi.less(vj)
	})
	return sorted[len(sorted)-maxCMTServerVersions:]
}

// handleCMTWithServerVersions orchestrates CMT testing and dispatches compatibility-matrix-testing.yml once.
func (s *Server) handleCMTWithServerVersions(repoOwner, repoName, instanceType, branch, sha string, serverVersions []string, runID int64, logger logrus.FieldLogger) {
	// Cap at maxCMTServerVersions. Auto-resolve already enforces this with ESR
	// preference; this is a backstop for a mis-set Config.CMTServerVersions override.
	if len(serverVersions) > maxCMTServerVersions {
		logger.Warnf("Capping server versions from %d to %d (keeping newest)", len(serverVersions), maxCMTServerVersions)
		serverVersions = capCMTServerVersions(serverVersions)
	}

	logger = logger.WithFields(logrus.Fields{
		"serverVersionCount": len(serverVersions),
		"branch":             branch,
		"sha":                sha,
		"run_id":             runID,
	})
	logger.Info("Starting CMT with server versions")

	// All-or-nothing: a partial matrix silently drops coverage, so roll back on any failure.
	var allInstances []*E2EInstance
	var validVersions []string

	for _, version := range serverVersions {
		version = strings.TrimSpace(version)
		if version == "" {
			continue
		}
		// Docker Hub tags use bare semver (e.g. "11.6.0"), not "v11.6.0".
		version = strings.TrimPrefix(version, "v")

		logger.WithField("version", version).Info("Creating CMT instances for server version")

		var versionInstances []*E2EInstance
		if instanceType == "mobile" {
			var err error
			versionInstances, err = s.createMobileCMTInstances(context.Background(), repoName, version, logger)
			if err != nil {
				logger.WithError(err).Errorf("Failed to create topology for version %s; rolling back partial CMT matrix", version)
				s.destroyE2EInstances(allInstances, logger)
				return
			}
		} else {
			instance, err := s.createSingleCMTInstance(context.Background(), repoName, instanceType, version, "", logger)
			if err != nil {
				logger.WithError(err).Errorf("Failed to create instance for version %s; rolling back partial CMT matrix", version)
				s.destroyE2EInstances(allInstances, logger)
				return
			}
			versionInstances = []*E2EInstance{instance}
		}
		expectedInstances := 1
		if instanceType == "mobile" {
			expectedInstances = len(mobileE2EPlatforms)
		}
		if len(versionInstances) != expectedInstances {
			logger.Errorf("Failed to create complete CMT topology for version %s; rolling back partial CMT matrix", version)
			s.destroyE2EInstances(append(allInstances, versionInstances...), logger)
			return
		}

		allInstances = append(allInstances, versionInstances...)
		validVersions = append(validVersions, version)
	}

	if len(allInstances) == 0 {
		logger.Warn("No CMT instances created (empty version set)")
		return
	}

	logger.WithField("totalInstances", len(allInstances)).Info("CMT instances created, dispatching test workflow")

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
		s.destroyE2EInstances(allInstances, logger)
		return
	}

	// Serialize per repo: concurrent triggers could race to the same run id without this mutex.
	dispatchLock := s.cmtDispatchMutex(repoName)
	dispatchLock.Lock()
	defer dispatchLock.Unlock()

	testRunID, err := s.dispatchCMTWorkflow(repoOwner, repoName, branch, cmtMatrixJSON, instanceType, logger)
	if err != nil {
		logger.WithError(err).Error("Failed to dispatch compatibility-matrix-testing.yml")
		s.destroyE2EInstances(allInstances, logger)
		return
	}

	if testRunID == 0 {
		// Dispatch succeeded but run id unresolved — leave instances for the periodic stale-scan.
		logger.WithField("trigger_run", runID).Warn("CMT dispatched but test run id unresolved; instances left to periodic stale-scan backstop")
		return
	}

	key := cmtInstanceKey(repoName, testRunID)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = allInstances
	s.e2eInstancesLock.Unlock()

	logger.WithFields(logrus.Fields{
		"tracking_key": key,
		"test_run_id":  testRunID,
		"trigger_run":  runID,
	}).Info("CMT workflow dispatched successfully; instances tracked for cleanup")
}

// createSingleCMTInstance creates one Mattermost cloud instance for a CMT server version.
func (s *Server) createSingleCMTInstance(ctx context.Context, repoName, instanceType, version, platform string, logger logrus.FieldLogger) (*E2EInstance, error) {
	sanitizedVersion := sanitizeForDNS(version)
	uid := e2eUniqueSuffix()
	nameParts := []string{instanceType, sanitizedVersion}
	if platform != "" {
		nameParts = append(nameParts, platform)
	}
	nameParts = append(nameParts, uid)
	name := e2eInstanceName(s.Config.DNSNameTestServer, nameParts...)

	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)

	instance, err := s.createCloudInstallation(ctx, name, version, username, password, instanceType, logger)
	if instance != nil {
		instance.Platform = platform
	}
	return instance, err
}

// createMobileCMTInstances creates one five-server topology for a server version.
func (s *Server) createMobileCMTInstances(ctx context.Context, repoName, version string, logger logrus.FieldLogger) ([]*E2EInstance, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		instance *E2EInstance
		err      error
	}
	results := make([]result, len(mobileE2EPlatforms))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, platform := range mobileE2EPlatforms {
		wg.Add(1)
		go func(idx int, platform string) {
			defer wg.Done()
			instance, err := s.createSingleCMTInstance(ctx, repoName, "mobile", version, platform, logger)
			results[idx] = result{instance: instance, err: err}
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
			}
		}(i, platform)
	}
	wg.Wait()

	instances := make([]*E2EInstance, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			continue
		}
		instances = append(instances, result.instance)
	}
	if firstErr != nil {
		s.destroyE2EInstances(instances, logger)
		return nil, firstErr
	}
	return instances, nil
}

// cmtServer is one entry in CMT_MATRIX. Mobile entries carry a five-server topology.
type cmtServer struct {
	Version         string `json:"version"`
	URL             string `json:"url,omitempty"`
	AndroidSite1URL string `json:"android_site_1_url,omitempty"`
	AndroidSite2URL string `json:"android_site_2_url,omitempty"`
	IOSSite1URL     string `json:"ios_site_1_url,omitempty"`
	IOSSite2URL     string `json:"ios_site_2_url,omitempty"`
	Site3URL        string `json:"site_3_url,omitempty"`
	Latest          bool   `json:"latest,omitempty"`
}

// buildDesktopCMTMatrixJSON builds CMT_MATRIX for compatibility-matrix-testing.yml: 3 fixed environment runners × N server versions.
func buildDesktopCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type cmtEnvironment struct {
		OS     string `json:"os"`
		Runner string `json:"runner"`
	}
	type desktopCMTMatrix struct {
		Environment []cmtEnvironment `json:"environment"`
		Server      []cmtServer      `json:"server"`
	}

	// macos-13 was retired by GitHub (queues forever). macos-26 matches desktop
	// PR E2E / CMT remapping. ubuntu-latest matches 24.04 (libasound2t64).
	matrix := desktopCMTMatrix{
		Environment: []cmtEnvironment{
			{OS: "linux", Runner: "ubuntu-latest"},
			{OS: "macos", Runner: "macos-26"},
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

// buildMobileCMTMatrixJSON builds one five-server topology per version, with the highest semver marked latest:true.
func buildMobileCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type mobileCMTMatrix struct {
		Server []cmtServer `json:"server"`
	}

	// Mark the highest-parseable version as latest; fall back to last entry if none parse.
	latestIdx := -1
	var latestVer cmtVersion
	requiredInstances := len(versions) * len(mobileE2EPlatforms)
	if len(instances) != requiredInstances {
		return "", fmt.Errorf("mobile CMT requires %d instances for %d versions, got %d", requiredInstances, len(versions), len(instances))
	}

	for i, version := range versions {
		v, ok := parseCMTVersion(version)
		if !ok {
			continue
		}
		if latestIdx == -1 || latestVer.less(v) {
			latestVer = v
			latestIdx = i
		}
	}
	if latestIdx == -1 && len(versions) > 0 {
		latestIdx = len(versions) - 1
	}

	var matrix mobileCMTMatrix
	for i, version := range versions {
		entry := cmtServer{Version: version}
		block := instances[i*len(mobileE2EPlatforms) : (i+1)*len(mobileE2EPlatforms)]
		platformToURL := make(map[string]string, len(block))
		for _, inst := range block {
			platformToURL[inst.Platform] = inst.URL
		}
		for _, platform := range mobileE2EPlatforms {
			url, ok := platformToURL[platform]
			if !ok {
				return "", fmt.Errorf("mobile CMT missing instance for platform %s in version %s", platform, version)
			}
			switch platform {
			case "android-site-1":
				entry.AndroidSite1URL = url
			case "android-site-2":
				entry.AndroidSite2URL = url
			case "ios-site-1":
				entry.IOSSite1URL = url
			case "ios-site-2":
				entry.IOSSite2URL = url
			case "site-3":
				entry.Site3URL = url
			}
		}
		if i == latestIdx {
			entry.Latest = true
		}
		matrix.Server = append(matrix.Server, entry)
	}

	b, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("failed to marshal mobile CMT matrix: %w", err)
	}
	return string(b), nil
}

// dispatchCMTWorkflow dispatches compatibility-matrix-testing.yml and polls for the run id used to key cleanup.
func (s *Server) dispatchCMTWorkflow(repoOwner, repoName, branch, cmtMatrixJSON, instanceType string, logger logrus.FieldLogger) (int64, error) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	// Snapshot run ids before dispatch so the poll only accepts strictly-new ids.
	preDispatchRunIDs, snapErr := s.listExistingCMTRunIDs(repoOwner, repoName, branch, logger)
	if snapErr != nil {
		// Non-fatal: poll will still accept any new id that appears after this moment.
		logger.WithError(snapErr).Warn("Failed to snapshot pre-dispatch CMT run ids; poll will only accept ids not seen before dispatch")
		preDispatchRunIDs = map[int64]bool{}
	}

	workflowInputs := map[string]interface{}{
		"CMT_MATRIX": cmtMatrixJSON,
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

	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/compatibility-matrix-testing.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    branch,
			"inputs": workflowInputs,
		})
	if err != nil {
		return 0, fmt.Errorf("failed to create CMT workflow dispatch request: %w", err)
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to dispatch compatibility-matrix-testing.yml: %w", err)
	}
	if resp.StatusCode != 204 {
		return 0, fmt.Errorf("unexpected status %d from compatibility-matrix-testing.yml dispatch", resp.StatusCode)
	}

	testRunID, err := s.pollDispatchedWorkflowRun(repoOwner, repoName, "compatibility-matrix-testing.yml", branch, preDispatchRunIDs, logger)
	if err != nil {
		// Return 0 to skip run-id tracking; leave instances for the periodic stale-scan.
		logger.WithError(err).Warn("Dispatched compatibility-matrix-testing.yml but could not resolve test run id within poll deadline")
		return 0, nil
	}

	logger.WithField("test_run_id", testRunID).Info("compatibility-matrix-testing.yml dispatched successfully")
	return testRunID, nil
}

// listExistingCMTRunIDs returns recent run ids for compatibility-matrix-testing.yml on branch, snapshotted before dispatch.
func (s *Server) listExistingCMTRunIDs(repoOwner, repoName, branch string, logger logrus.FieldLogger) (map[int64]bool, error) {
	runs, err := s.listCMTRuns(repoOwner, repoName, "compatibility-matrix-testing.yml", branch)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]bool, len(runs))
	for _, run := range runs {
		ids[run.ID] = true
	}
	logger.WithField("pre_dispatch_run_count", len(ids)).Debug("Snapshotted pre-dispatch CMT run ids")
	return ids, nil
}

// cmtWorkflowRun is the minimal slice of the GitHub workflow_run object we need.
type cmtWorkflowRun struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

// listCMTRuns fetches up to the 10 most-recent workflow_dispatch runs for workflowFile on branch.
func (s *Server) listCMTRuns(repoOwner, repoName, workflowFile, branch string) ([]cmtWorkflowRun, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	if s.githubAPIBase != "" {
		if baseURL, parseErr := url.Parse(s.githubAPIBase); parseErr == nil {
			client.BaseURL = baseURL
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	listURL := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?event=workflow_dispatch&branch=%s&per_page=10",
		repoOwner, repoName, workflowFile, url.QueryEscape(branch))
	req, err := client.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow runs list request: %w", err)
	}
	var resp struct {
		WorkflowRuns []cmtWorkflowRun `json:"workflow_runs"`
	}
	if _, err = client.Do(ctx, req, &resp); err != nil {
		return nil, fmt.Errorf("failed to list workflow runs: %w", err)
	}
	return resp.WorkflowRuns, nil
}

// pollDispatchedWorkflowRun polls for a new run id not in preDispatchRunIDs. The pre-dispatch snapshot makes this race-free vs. time-window approaches.
func (s *Server) pollDispatchedWorkflowRun(repoOwner, repoName, workflowFile, branch string, preDispatchRunIDs map[int64]bool, logger logrus.FieldLogger) (int64, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.listCMTRuns(repoOwner, repoName, workflowFile, branch)
		if err != nil {
			return 0, err
		}

		// Also skip ids already tracked to guard against concurrent dispatches overwriting a stored entry.
		claimed := s.claimedCMTRunIDs(repoName)

		var bestID int64
		var bestCreated time.Time
		for _, run := range runs {
			if preDispatchRunIDs[run.ID] {
				continue
			}
			if claimed[run.ID] {
				continue
			}
			createdAt, parseErr := time.Parse(time.RFC3339, run.CreatedAt)
			if parseErr != nil {
				continue
			}
			if run.ID > bestID {
				bestID = run.ID
				bestCreated = createdAt
			}
		}
		if bestID != 0 {
			logger.WithFields(logrus.Fields{
				"test_run_id": bestID,
				"created_at":  bestCreated,
			}).Debug("Resolved dispatched CMT test workflow run")
			return bestID, nil
		}

		time.Sleep(2 * time.Second)
	}

	return 0, fmt.Errorf("timed out polling for dispatched workflow run on branch %s", branch)
}

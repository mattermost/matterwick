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
	"strings"
	"sync"
	"time"

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
	// per version in the resolved CMT set (s.cmtServerVersions) and dispatches
	// compatibility-matrix-testing.yml. No inputs are read from the event — the version set is
	// resolved by matterwick — so this works despite GitHub's workflow_run payload not carrying
	// workflow_dispatch inputs.
	// Cleanup happens when the test workflow ("Compatibility Matrix Testing") completes, handled
	// by the isE2ETestWorkflow branch below.
	if s.Config.CMTTriggerWorkflowName != "" && workflowName == s.Config.CMTTriggerWorkflowName {
		if payload.Action == "requested" {
			// Gate CMT to the cases we actually want. The trigger workflow fires on RC tag pushes
			// (e.g. `v6.2.0-rc.1`) and on manual dispatch. For tag pushes the workflow_run
			// payload's head_branch carries the *tag name*, not a branch. Provision CMT when:
			//   - the run was started manually (workflow_dispatch, any ref), OR
			//   - head_branch is an RC tag (isRCTag), OR
			//   - head_branch is a release branch (defense-in-depth for legacy/manual triggers).
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

	// --- Test workflow completion: clean up provisioned instances ---
	//
	// CMT keys on the dispatched *test* workflow run id ({repo}-cmt-{testRunID}) so
	// concurrent re-spins on the same branch HEAD cannot cross-destroy each other's servers
	// when GitHub concurrency cancels an older run. Non-CMT flows (push/scheduled) still
	// key on head_sha suffix; orphans are reaped by cleanupStaleE2EInstances.
	if payload.Action == "completed" && s.isE2ETestWorkflow(workflowName) {
		if workflowName == cmtTestWorkflowName {
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

	instances, err := s.createCMTInstancesForVersion(repoName, instanceType, s.resolveMattermostServerVersion(), "nightly")
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
	if triggerEvent != "schedule" {
		if branch == "master" || branch == "main" {
			runType = "MASTER"
		} else if s.isReleaseBranch(branch) {
			runType = "RELEASE"
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
		dispatchErr = s.dispatchDesktopE2EWorkflow(owner, repoName, branch, sha, instanceDetailsJSON, runType)
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

// cmtTestWorkflowName is the workflow "name:" of compatibility-matrix-testing.yml in the
// desktop and mobile repos.
const cmtTestWorkflowName = "Compatibility Matrix Testing"

// cmtInstanceKey is the in-memory tracking key for a CMT test workflow run.
func cmtInstanceKey(repoName string, testRunID int64) string {
	return fmt.Sprintf("%s-cmt-%d", repoName, testRunID)
}

// cmtDispatchMutex returns a per-repository mutex that serializes the CMT
// dispatch+poll+store critical section so two near-simultaneous dispatches cannot
// resolve to the same test workflow run id in pollDispatchedWorkflowRun.
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

// claimedCMTRunIDs returns the set of CMT test run ids currently tracked for repoName.
// Used by pollDispatchedWorkflowRun to skip ids already claimed by a previous dispatch.
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

// removeCMTInstancesByRunID atomically removes and returns the CMT instance set keyed to
// the completing compatibility-matrix-testing.yml run id. Returns nil for runID 0 (the
// "could not resolve" sentinel from pollDispatchedWorkflowRun) so unresolved dispatches
// cannot accidentally wipe a real entry, and nil when the key has no entry.
//
// Split from findAndDestroyInstancesByRunID so tests can exercise the locking, filtering,
// and return shape without touching CloudClient.
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

// instanceKeyMatchesSHA reports whether a tracking key belongs to repoName, ends with the given
// head SHA, and matches the requested flow: CMT keys ("{repo}-cmt-…") when cmtOnly is true,
// non-CMT keys (push/scheduled) when false. This scoping prevents a completing workflow from
// reaping a different flow's run that happens to share the same commit SHA (e.g. the monthly CMT
// trigger and the nightly trigger firing on the same master commit).
func instanceKeyMatchesSHA(key, repoName, headSHA string, cmtOnly bool) bool {
	if !strings.HasPrefix(key, repoName+"-") || !strings.HasSuffix(key, "-"+headSHA) {
		return false
	}
	return strings.HasPrefix(key, repoName+"-cmt-") == cmtOnly
}

// findAndDestroyInstancesBySHA scans the instance map for entries belonging to repoName whose
// tracking key ends with "-{headSHA}" and destroys them. When cmtOnly is true only CMT keys
// ("{repo}-cmt-…") match; when false only non-CMT keys (push/scheduled) match — so a completing
// workflow never tears down a different flow's run that happens to share the same SHA.
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

// shouldTriggerCMT decides whether a CMT-trigger workflow_run should actually provision CMT.
// CMT runs only when:
//   - the run was started manually (workflow_dispatch, any ref), OR
//   - head_branch is an RC tag (desktop convention: v6.2.0-rc.1), OR
//   - head_branch is a mobile release-build branch (build-release-NNN, 3+ digits), OR
//   - head_branch is a release branch (defense-in-depth — legacy & for any manual ref).
//
// For tag-push events GitHub sets workflow_run.head_branch to the tag name (no refs/tags/
// prefix), so an RC tag like "v6.2.0-rc.1" arrives in headBranch.
func (s *Server) shouldTriggerCMT(triggerEvent, headBranch string) bool {
	return triggerEvent == "workflow_dispatch" ||
		isRCTag(headBranch) ||
		isBuildReleaseBranch(headBranch) ||
		s.isReleaseBranch(headBranch)
}

// rcTagPattern matches RC tags like "v6.2.0-rc.1", "v2.41.0-rc.10", "6.2.0-rc.1". The leading
// "v" is optional, and the rc suffix can be "-rc.N", "-rcN", or "-rc-N" (we keep it permissive
// but require the literal "-rc" and a trailing number). Compiled once at package init.
var rcTagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+-rc[.\-]?\d+$`)

// isRCTag reports whether ref looks like a release-candidate tag we want to trigger CMT on.
// Excludes GA tags (v6.2.0), beta/alpha pre-releases (v1.0.22-beta), and nightly tags
// (6.3.0-nightly.20260601).
func isRCTag(ref string) bool {
	return rcTagPattern.MatchString(ref)
}

// buildReleaseBranchPattern matches mobile's release-build branches: "build-release-" followed
// by 3 or more digits (current convention is 3- or 4-digit build numbers like 786 / 1100).
// 3+ digits is intentional — rejects accidental test branches like "build-release-1" without
// locking an upper bound, so a future move to 5-digit build numbers needs no code change.
// Platform-specific variants (build-release-ios-NNN, build-release-sim-NNN,
// build-release-android-NNN) don't start with a digit after the "build-release-" prefix and
// are excluded.
var buildReleaseBranchPattern = regexp.MustCompile(`^build-release-\d{3,}$`)

// isBuildReleaseBranch reports whether ref matches mobile's build-release-NNN convention.
// The branch is created by the "Mattermost Mobile Release" external workflow when an RC
// build is dispatched to TestFlight / Play Store, so it's mobile's equivalent of an RC tag
// cut. Used as a separate gate from isReleaseBranch because release-* fires on every
// cherry-pick / version-bump push, which would be far too noisy for CMT.
func isBuildReleaseBranch(ref string) bool {
	return buildReleaseBranchPattern.MatchString(ref)
}

// handleCMTTrigger is invoked when the scheduled CMT trigger workflow fires. It resolves the
// instance type from the repo, resolves the server-version set (auto-derived from Mattermost
// releases, or the manual Config.CMTServerVersions override), and hands off to
// handleCMTWithServerVersions. Nothing needs to be read from the workflow_run event.
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

// handleCMTWithServerVersions orchestrates CMT testing: creates one instance per server
// version, builds the CMT_MATRIX JSON, and dispatches compatibility-matrix-testing.yml once.
func (s *Server) handleCMTWithServerVersions(repoOwner, repoName, instanceType, branch, sha string, serverVersions []string, runID int64, logger logrus.FieldLogger) {
	// Cap the number of versions (one cloud instance per version) to prevent runaway provisioning.
	const maxVersions = 10
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
	// so a single server URL handles all platform test runners for that version. All-or-
	// nothing: if any version fails, destroy what was already created — a partial matrix
	// would silently drop coverage and the operator wouldn't see it on the commit status.
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
			logger.WithError(err).Errorf("Failed to create instance for version %s; rolling back partial CMT matrix", version)
			s.destroyE2EInstances(allInstances, logger)
			return
		}

		allInstances = append(allInstances, instance)
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

	// Serialize dispatch+poll+store per repo. Without this, two concurrent CMT triggers on
	// the same repo can have overlapping poll windows in pollDispatchedWorkflowRun and both
	// resolve to the same test workflow run id, causing the second store to overwrite the
	// first entry and leak the first run's instances to the periodic DNS scan.
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
		// GitHub accepted the dispatch (204) but we could not resolve the queued run id
		// within the poll deadline. Do NOT destroy instances — the workflow may still run
		// against them. The periodic DNS-pattern scan will reap them after E2EInstanceMaxAge
		// (configurable; default scan applies). Operator must investigate the warning.
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

// cmtServer is the server entry in CMT_MATRIX JSON. Mobile uses the optional `latest` flag
// to mark the highest-semver entry, so the mobile workflow can decide to run the whole suite
// against the latest server and smoke-only against older ones. Desktop ignores it and the
// `omitempty` keeps desktop's matrix JSON unchanged (the field is just absent there).
//
// IMPORTANT — layering: matterwick only signals which server is "latest". It does NOT decide
// what tests run for latest vs non-latest — that policy (smoke path vs whole suite path,
// parallelism, etc.) lives in the mobile workflow, where the test-directory structure is
// known.
type cmtServer struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Latest  bool   `json:"latest,omitempty"`
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
// in the mobile repo. One iOS test job is created per server version. The highest-semver
// entry is marked `latest: true` so the mobile workflow can branch on it (e.g. whole suite
// for latest, smoke for older). What "latest" implies test-wise is the workflow's policy,
// not matterwick's — see the cmtServer doc comment.
//
// Schema:
//
//	{
//	  "server": [
//	    {"version": "10.11.18", "url": "https://..."},
//	    {"version": "11.7.1",   "url": "https://...", "latest": true}
//	  ]
//	}
func buildMobileCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type mobileCMTMatrix struct {
		Server []cmtServer `json:"server"`
	}

	// Find the highest-semver entry across versions that parse successfully. parseCMTVersion
	// handles vX.Y.Z and vX.Y.Z-rcN (with or without the "v"). Unparseable versions are
	// silently skipped — they just don't compete for "latest". If none parse, mark the last
	// entry so maestro legs are not all skipped and final status does not false-fail.
	latestIdx := -1
	var latestVer cmtVersion
	for i, version := range versions {
		if i >= len(instances) {
			break
		}
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
		if i >= len(instances) {
			break
		}
		entry := cmtServer{Version: version, URL: instances[i].URL}
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

// dispatchCMTWorkflow dispatches compatibility-matrix-testing.yml with the populated
// CMT_MATRIX JSON, targeting the branch ref (workflow_dispatch requires a branch/tag, not a SHA),
// then polls the GitHub API for the queued test run id used to key instance cleanup.
func (s *Server) dispatchCMTWorkflow(repoOwner, repoName, branch, cmtMatrixJSON, instanceType string, logger logrus.FieldLogger) (int64, error) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	// Snapshot existing workflow_dispatch run ids BEFORE dispatching. The poll then
	// returns the newest run id NOT in this set — eliminating the time-window race
	// where an older manual or completed workflow_dispatch run on the same branch
	// could win the poll if our newly dispatched run hasn't appeared yet.
	preDispatchRunIDs, snapErr := s.listExistingCMTRunIDs(repoOwner, repoName, branch, logger)
	if snapErr != nil {
		// Failing to snapshot is not fatal — fall back to "everything is pre-existing"
		// which forces the poll to wait for a strictly-new run id. Worst case is
		// a longer poll if the new dispatch is slow to show up.
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

	// GitHub workflow_dispatch requires a branch or tag name as ref, not a commit SHA.
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
		// Dispatch already succeeded (204). Returning (0, nil) tells the caller to skip
		// SHA→runID-keyed cleanup tracking but to LEAVE instances in place — the workflow
		// may still execute against them. The periodic stale-scan is the backstop.
		logger.WithError(err).Warn("Dispatched compatibility-matrix-testing.yml but could not resolve test run id within poll deadline")
		return 0, nil
	}

	logger.WithField("test_run_id", testRunID).Info("compatibility-matrix-testing.yml dispatched successfully")
	return testRunID, nil
}

// listExistingCMTRunIDs snapshots the set of recent workflow_dispatch run ids for
// compatibility-matrix-testing.yml on branch. Used immediately before dispatch so the
// post-dispatch poll can ignore any id that already existed.
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

// pollDispatchedWorkflowRun polls until GitHub lists a workflow_dispatch run for branch
// whose id was NOT present in preDispatchRunIDs (snapshotted just before the dispatch call).
// This is race-free vs. a time-window approach: even if an older manual dispatch is still
// listed, its id was already in the pre-dispatch snapshot and is filtered out.
func (s *Server) pollDispatchedWorkflowRun(repoOwner, repoName, workflowFile, branch string, preDispatchRunIDs map[int64]bool, logger logrus.FieldLogger) (int64, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.listCMTRuns(repoOwner, repoName, workflowFile, branch)
		if err != nil {
			return 0, err
		}

		// Skip run ids already tracked in e2eInstances — defense in depth so a hypothetical
		// concurrent dispatch (or a re-poll after a non-clean restart) cannot resolve to the
		// same id as a previously-stored entry and overwrite it.
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

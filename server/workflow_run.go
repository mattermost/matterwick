// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
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
	if s.cmtDispatchLocks == nil {
		s.cmtDispatchLocks = make(map[string]*sync.Mutex)
	}
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

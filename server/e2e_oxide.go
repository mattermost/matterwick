// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/google/go-github/v32/github"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

// Oxide E2E profile (mattermost/mattermost-mobile-oxide).
//
// The repo name contains "mobile", so every desktop/mobile substring check would
// otherwise treat it as mattermost-mobile: provision mobile-pr-<N>-* servers
// (colliding with mobile PR numbers), dispatch e2e-detox-pr.yml (absent in Oxide),
// and on PR close delete mattermost-mobile's servers for the same PR number.
// Every E2E entry point therefore checks isOxideRepo first. The profile only
// activates when Config.E2EOxideEnabled is true; otherwise Oxide events are ignored.
//
// Two flows use it: PR labels (oxide-pr-<N>-* servers, reused until PR close) and pushes
// to the default branch (oxide-main-* servers, fresh per push and destroyed when the
// dispatched run completes, as for mattermost-mobile's master pushes). Oxide has no CMT.

const (
	oxideRepoName = "mattermost-mobile-oxide"

	// oxideE2EInstanceType is the instance type and DNS prefix (oxide-pr-<N>-<platform>-<uid>).
	// It must not share a prefix with "mobile" or "desktop" so DNS LIKE patterns stay disjoint.
	oxideE2EInstanceType = "oxide"

	// oxideE2EWorkflowFile is dispatched on the PR head ref, or on the default branch for
	// pushes. Its "name:" must not be in E2ETestWorkflowNames: PR servers are reused across
	// label toggles until PR close / E2E/Reset-Servers / max age, so completion cleanup
	// matches this file's runs by path and only ever destroys the exact push key.
	oxideE2EWorkflowFile = "e2e-matterwick.yml"

	// oxidePushE2ENameToken replaces "pr-<N>" in push server names (oxide-main-<platform>-<uid>),
	// so push servers never match a PR's oxide-pr-<N>-% pattern or the scan's "-pr-" check.
	oxidePushE2ENameToken = "main"
)

// oxideE2EPlatforms are the server slots Oxide provisions per PR: three per mobile OS.
// The Oxide workflow spreads its Maestro shards across the three servers of each OS
// (each shard runs as its own user/team, so shards sharing a server do not interfere).
var oxideE2EPlatforms = []string{
	"android-site-1",
	"android-site-2",
	"android-site-3",
	"ios-site-1",
	"ios-site-2",
	"ios-site-3",
}

// oxideE2EWorkflowInputKeys pairs index-for-index with oxideE2EPlatforms.
var oxideE2EWorkflowInputKeys = []string{
	"ANDROID_SITE_1_URL",
	"ANDROID_SITE_2_URL",
	"ANDROID_SITE_3_URL",
	"IOS_SITE_1_URL",
	"IOS_SITE_2_URL",
	"IOS_SITE_3_URL",
}

// isOxideRepo reports whether repoName is the Oxide repo (exact name, case-insensitive).
func isOxideRepo(repoName string) bool {
	return strings.EqualFold(repoName, oxideRepoName)
}

// buildURLInputs maps each instance's Platform to its workflow input key. It requires
// exactly one instance per platform so dispatch inputs never drift from the topology.
func buildURLInputs(kind string, instances []*E2EInstance, platforms, keys []string) (map[string]string, error) {
	if len(platforms) != len(keys) {
		return nil, fmt.Errorf("%s E2E topology mismatch: %d platforms, %d input keys", kind, len(platforms), len(keys))
	}
	if len(instances) != len(platforms) {
		return nil, fmt.Errorf("%s E2E requires exactly %d instances, got %d", kind, len(platforms), len(instances))
	}
	platformToURL := make(map[string]string, len(instances))
	for _, inst := range instances {
		platformToURL[inst.Platform] = inst.URL
	}
	inputs := make(map[string]string, len(keys))
	for i, platform := range platforms {
		url, ok := platformToURL[platform]
		if !ok {
			return nil, fmt.Errorf("%s E2E missing instance for platform %s", kind, platform)
		}
		inputs[keys[i]] = url
	}
	return inputs, nil
}

// triggerOxideE2EWorkflow dispatches e2e-matterwick.yml for a PR, on its head ref.
func (s *Server) triggerOxideE2EWorkflow(ctx context.Context, client *github.Client, pr *model.PullRequest, instances []*E2EInstance, testPlatform string) error {
	return s.dispatchOxideE2EWorkflow(ctx, client, pr.RepoOwner, pr.RepoName, pr.Ref, pr.Sha, fmt.Sprintf("%d", pr.Number), instances, testPlatform)
}

// dispatchOxideE2EWorkflow dispatches e2e-matterwick.yml in the Oxide repo. An empty
// prNumber marks a default-branch run (statuses on the commit, no label, queued runs).
// Inputs must exactly match the workflow's declared workflow_dispatch inputs:
// GitHub rejects undeclared inputs with 422 and allows at most 10.
func (s *Server) dispatchOxideE2EWorkflow(ctx context.Context, client *github.Client, repoOwner, repoName, ref, sha, prNumber string, instances []*E2EInstance, testPlatform string) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         repoName,
		"ref":          ref,
		"pr":           prNumber,
		"type":         oxideE2EInstanceType,
		"testPlatform": testPlatform,
	})

	urlInputs, err := buildURLInputs(oxideE2EInstanceType, instances, oxideE2EPlatforms, oxideE2EWorkflowInputKeys)
	if err != nil {
		return err
	}

	inputs := map[string]interface{}{
		// The workflow's plan job rejects any MOBILE_VERSION other than the dispatched ref's commit.
		"MOBILE_VERSION": sha,
		"PLATFORM":       testPlatform, // ios/android/both
		"pr_number":      prNumber,
	}
	for key, url := range urlInputs {
		inputs[key] = url
	}

	body := map[string]interface{}{
		"ref":    ref,
		"inputs": inputs,
	}

	logger.WithField("workflow", oxideE2EWorkflowFile).Debug("Triggering Oxide E2E workflow")

	req, err := client.NewRequest("POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", repoOwner, repoName, oxideE2EWorkflowFile), body)
	if err != nil {
		return fmt.Errorf("failed to create workflow dispatch request: %w", err)
	}

	if _, err = client.Do(ctx, req, nil); err != nil {
		return fmt.Errorf("failed to trigger oxide e2e workflow: %w", err)
	}

	logger.Info("Successfully triggered Oxide E2E workflow")
	return nil
}

// oxidePushE2ESkipReason returns why a push to the Oxide repo must not run E2E, or "" when
// it should: the profile and master/main auto-trigger are on and the push is to the
// repo's default branch.
func (s *Server) oxidePushE2ESkipReason(event *github.PushEvent, branch string) string {
	switch {
	case !s.Config.E2EOxideEnabled:
		return "Oxide E2E is disabled (E2EOxideEnabled=false)"
	case !s.Config.E2EAutoTriggerOnMaster:
		return "push E2E is disabled (E2EAutoTriggerOnMaster=false)"
	case event.GetDeleted():
		return "branch was deleted"
	case event.GetRepo().GetDefaultBranch() == "" || branch != event.GetRepo().GetDefaultBranch():
		return "push is not to the default branch"
	}
	return ""
}

// oxidePushInstanceKey is the e2eInstances key of a push run's servers. Completion cleanup
// rebuilds it from the run's head_branch and head_sha, so it only ever matches that run.
func oxidePushInstanceKey(repoName, branch, sha string) string {
	return fmt.Sprintf("%s-push-%s-%s", repoName, branch, sha)
}

// isOxideE2EWorkflowRun reports whether a workflow_run is e2e-matterwick.yml in the Oxide repo.
// workflowPath is the run's "path" (".github/workflows/e2e-matterwick.yml", possibly "@ref").
func isOxideE2EWorkflowRun(repoName, workflowPath string) bool {
	workflowPath = strings.SplitN(workflowPath, "@", 2)[0]
	return isOxideRepo(repoName) && path.Base(workflowPath) == oxideE2EWorkflowFile
}

// handleOxidePushEventE2E provisions fresh oxide-main-* servers for a push to the default
// branch and dispatches e2e-matterwick.yml on that branch with PLATFORM=both and no
// pr_number. The servers are destroyed when that run completes (handleWorkflowRunEventWithInputs).
func (s *Server) handleOxidePushEventE2E(event *github.PushEvent, branch string) {
	repoName := event.GetRepo().GetName()
	sha := event.GetHeadCommit().GetID()

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
		"sha":    sha,
		"type":   oxideE2EInstanceType,
	})

	if sha == "" {
		logger.Error("Push event has no commit SHA, skipping Oxide E2E dispatch")
		return
	}
	repoOwner := s.Config.Org
	if repoOwner == "" {
		logger.Error("Organization not configured, skipping Oxide E2E dispatch")
		return
	}

	instances, err := s.createMultipleE2EInstancesForPushEvent(repoName, oxideE2EInstanceType, branch)
	if err != nil {
		logger.WithError(err).Error("Failed to create Oxide E2E instances")
		s.logErrorToMattermost("E2E on %s %s (%s) did not run: failed to provision test servers (%v)", repoName, branch, sha, err)
		return
	}

	s.dispatchOxidePushRun(repoOwner, repoName, branch, sha, instances, logger)
}

// dispatchOxidePushRun tracks the push's servers and dispatches its run, or destroys the
// servers when the run is superseded or cannot be dispatched.
func (s *Server) dispatchOxidePushRun(repoOwner, repoName, branch, sha string, instances []*E2EInstance, logger logrus.FieldLogger) {
	// The workflow rejects a MOBILE_VERSION that is not the dispatched branch's HEAD. If the
	// branch moved on while the servers came up, the newer push runs its own E2E; this one
	// is superseded. On a lookup error, dispatch anyway (the workflow's check is authoritative).
	head, err := s.resolveBranchHeadSHA(repoOwner, repoName, branch)
	if err != nil {
		logger.WithError(err).Warn("Failed to resolve branch HEAD SHA; dispatching with the push SHA")
	} else if head != sha {
		logger.WithField("head_sha", head).Info("Branch moved on during provisioning; the newer push runs Oxide E2E, destroying this push's servers")
		s.destroyE2EInstances(instances, logger)
		return
	}

	// Store before dispatching so a fast-completing run doesn't race ahead of tracking.
	key := oxidePushInstanceKey(repoName, branch, sha)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	if err := s.dispatchOxideE2EWorkflow(context.Background(), s.e2eGithubClient(), repoOwner, repoName, branch, sha, "", instances, "both"); err != nil {
		logger.WithError(err).Error("Failed to trigger Oxide E2E workflow")
		s.logErrorToMattermost("E2E on %s %s (%s) did not run: workflow dispatch failed (%v)", repoName, branch, sha, err)
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.Info("Oxide E2E workflow triggered for push; instances tracked for cleanup on completion")
}

// destroyOxidePushInstances destroys the servers of the push run that just completed.
func (s *Server) destroyOxidePushInstances(repoName, headBranch, headSHA string, logger logrus.FieldLogger) {
	if headBranch == "" || headSHA == "" {
		return
	}
	key := oxidePushInstanceKey(repoName, headBranch, headSHA)
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) == 0 {
		logger.WithField("tracking_key", key).Debug("No Oxide push instances tracked for this run")
		return
	}
	logger.WithFields(logrus.Fields{
		"tracking_key": key,
		"instances":    len(instances),
	}).Info("Oxide E2E run completed, destroying its push instances")
	s.destroyE2EInstances(instances, logger)
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"strings"

	"github.com/google/go-github/v32/github"
	"github.com/sirupsen/logrus"
)

// handlePushEvent runs E2E for a push to an E2E repo's default branch.
func (s *Server) handlePushEvent(event *github.PushEvent) {
	repoName := event.GetRepo().GetName()
	branchRef := event.GetRef()

	// Ignore tag pushes (refs/tags/*) — only branch pushes should trigger E2E.
	if !strings.HasPrefix(branchRef, "refs/heads/") {
		s.Logger.WithFields(logrus.Fields{
			"repo":   repoName,
			"ref":    branchRef,
			"action": "push",
		}).Info("Push ref is not a branch, skipping E2E trigger")
		return
	}

	branch := extractBranchName(branchRef)
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
		"action": "push",
	})

	p := e2eProfileFor(repoName)
	if reason := s.pushE2ESkipReason(p, event, branch); reason != "" {
		logger.WithField("reason", reason).Info("Push event does not trigger E2E")
		return
	}
	logger.WithField("kind", p.kind).Info("Default branch push detected, triggering E2E tests")
	go s.handlePushEventE2E(p, event, branch)
}

// pushE2ESkipReason returns why a push must not run E2E, or "" when it should: the repo has
// an enabled profile, push E2E is on, and the push is to the repo's default branch.
func (s *Server) pushE2ESkipReason(p *e2eProfile, event *github.PushEvent, branch string) string {
	switch {
	case p == nil:
		return "repository has no E2E profile"
	case !s.e2eEnabled(p):
		return "E2E is disabled for this repository"
	case !s.Config.E2EAutoTriggerOnMaster:
		return "push E2E is disabled (E2EAutoTriggerOnMaster=false)"
	case event.GetDeleted():
		return "branch was deleted"
	case event.GetRepo().GetDefaultBranch() == "" || branch != event.GetRepo().GetDefaultBranch():
		return "push is not to the default branch"
	}
	return ""
}

// isReleaseBranch returns true if branch matches E2EReleasePatternPrefix.
// Rejects empty prefix — strings.HasPrefix(x, "") is always true.
func (s *Server) isReleaseBranch(branch string) bool {
	if s.Config.E2EReleasePatternPrefix == "" {
		return false
	}
	return strings.HasPrefix(branch, s.Config.E2EReleasePatternPrefix)
}

// extractBranchName extracts the branch name from "refs/heads/branch-name".
func extractBranchName(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) < 3 {
		return ref
	}
	return strings.Join(parts[2:], "/")
}

// handlePushEventE2E provisions fresh <kind>-push-* servers for a default-branch push and
// dispatches the profile's workflow on that branch (PLATFORM=both). The servers are
// destroyed when that run completes (handleWorkflowRunEventWithInputs).
func (s *Server) handlePushEventE2E(p *e2eProfile, event *github.PushEvent, branch string) {
	run := e2eRun{
		owner:    s.Config.Org,
		repo:     event.GetRepo().GetName(),
		ref:      branch,
		sha:      event.GetHeadCommit().GetID(),
		platform: "both",
	}
	logger := s.Logger.WithFields(logrus.Fields{"repo": run.repo, "branch": branch, "sha": run.sha, "kind": p.kind})

	if run.sha == "" || run.owner == "" {
		logger.Error("Push event has no commit SHA or Org is not configured, skipping E2E dispatch")
		return
	}

	instances, err := s.provisionRun(p, run, logger)
	if err != nil {
		// Push E2E has no PR comment to fall back on, so a provisioning failure is invisible
		// unless it is reported.
		logger.WithError(err).Error("Failed to create E2E instances")
		s.logErrorToMattermost("E2E on %s %s (%s) did not run: failed to provision test servers (%v)", run.repo, branch, run.sha, err)
		return
	}

	s.dispatchPushRun(p, run, instances, logger)
}

// dispatchPushRun tracks a push run's servers and dispatches it, or destroys the servers when
// the run is superseded or cannot be dispatched.
func (s *Server) dispatchPushRun(p *e2eProfile, run e2eRun, instances []*E2EInstance, logger logrus.FieldLogger) {
	// The dispatched run tests the branch HEAD. If the branch moved on while the servers came
	// up, the newer push runs its own E2E and this one is superseded. On a lookup error,
	// dispatch anyway.
	head, err := s.resolveBranchHeadSHA(run.owner, run.repo, run.ref)
	if err != nil {
		logger.WithError(err).Warn("Failed to resolve branch HEAD SHA; dispatching with the push SHA")
	} else if head != run.sha {
		logger.WithField("head_sha", head).Info("Branch moved on during provisioning; the newer push runs E2E, destroying this push's servers")
		s.destroyE2EInstances(instances, logger)
		return
	}

	// Store before dispatching so a fast-completing run doesn't race ahead of tracking.
	key := run.key()
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	if err := s.dispatchE2E(p, run, instances); err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		s.logErrorToMattermost("E2E on %s %s (%s) did not run: workflow dispatch failed (%v)", run.repo, run.ref, run.sha, err)
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(instances, logger)
	}
}

// destroyPushRunInstances destroys the servers of the push run that just completed.
func (s *Server) destroyPushRunInstances(repoName, headBranch, headSHA string, logger logrus.FieldLogger) {
	if headBranch == "" || headSHA == "" {
		return
	}
	key := pushRunKey(repoName, headBranch, headSHA)
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) == 0 {
		logger.WithField("tracking_key", key).Debug("No push instances tracked for this run")
		return
	}
	logger.WithFields(logrus.Fields{"tracking_key": key, "instances": len(instances)}).Info("E2E run completed, destroying its push instances")
	s.destroyE2EInstances(instances, logger)
}

// getRunnerForPlatform returns the runner label for E2E functional tests. CMT uses a separate hardcoded matrix with pinned OS versions.
func getRunnerForPlatform(platform string) string {
	switch strings.ToLower(platform) {
	case "linux":
		return "ubuntu-latest"
	case "macos":
		return "macos-latest"
	case "windows":
		return "windows-2022"
	default:
		return "ubuntu-latest"
	}
}

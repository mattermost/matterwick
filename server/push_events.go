// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/go-github/v32/github"
	"github.com/sirupsen/logrus"
)

// handlePushEvent triggers E2E tests on release branches or master/main pushes.
func (s *Server) handlePushEvent(event *github.PushEvent) {
	repoName := event.GetRepo().GetName()
	branchRef := event.GetRef()

	// Ignore tag pushes (refs/tags/*) — only branch pushes should trigger E2E.
	// Without this guard, a tag like "refs/tags/release-9.0" would pass through
	// extractBranchName as "release-9.0" and accidentally match isReleaseBranch.
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
	logger.Info("Push event received")

	// Release-branch push trigger was removed; release stabilization is covered by PR-label E2E and CMT.

	if s.Config.E2EAutoTriggerOnMaster && (branch == "master" || branch == "main") {
		logger.WithField("type", "master_main").Info("Master/main branch detected, triggering E2E tests")
		go s.handlePushEventE2E(event, branch)
		return
	}

	logger.WithField("auto_master", s.Config.E2EAutoTriggerOnMaster).
		Info("Push event does not match E2E trigger conditions")
}

// isReleaseBranch returns true if branch matches E2EReleasePatternPrefix.
// Rejects empty prefix — strings.HasPrefix(x, "") is always true.
func (s *Server) isReleaseBranch(branch string) bool {
	if s.Config.E2EReleasePatternPrefix == "" {
		return false
	}
	return strings.HasPrefix(branch, s.Config.E2EReleasePatternPrefix)
}

func (s *Server) serverVersionForPushEvent() string {
	return s.resolveMattermostServerVersion()
}

// extractBranchName extracts the branch name from "refs/heads/branch-name".
func extractBranchName(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) < 3 {
		return ref
	}
	return strings.Join(parts[2:], "/")
}

// handlePushEventE2E provisions E2E servers and dispatches the test workflow
// for a push to a release branch or master/main. Only acts on desktop/mobile repos.
func (s *Server) handlePushEventE2E(event *github.PushEvent, branch string) {
	repoName := event.GetRepo().GetName()
	commit := event.GetHeadCommit()
	sha := ""
	if commit != nil {
		sha = commit.GetID()
	}

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
		"sha":    sha,
	})

	isDesktop := strings.Contains(repoName, "desktop")
	isMobile := strings.Contains(repoName, "mobile")

	if !isDesktop && !isMobile {
		logger.Warn("Repository is neither desktop nor mobile, skipping E2E tests")
		return
	}

	instanceType := "desktop"
	if isMobile {
		instanceType = "mobile"
	}

	if sha == "" {
		logger.Error("Push event has no commit SHA, skipping E2E dispatch")
		return
	}

	logger.WithField("instanceType", instanceType).Info("Creating E2E instances for push event")

	instances, err := s.createMultipleE2EInstancesForPushEvent(repoName, instanceType, branch)
	if err != nil {
		logger.WithError(err).Error("Failed to create E2E instances")
		return
	}

	if len(instances) == 0 {
		logger.Error("No instances created for E2E testing")
		return
	}

	logger.WithField("instanceCount", len(instances)).Info("E2E instances created successfully")

	// Key on the branch HEAD resolved now (just before dispatch), not the push SHA: the
	// dispatched (ref=branch) run reports its head_sha as the branch HEAD at dispatch time,
	// which can differ from the push SHA if the branch advanced during provisioning. The key
	// still ends with "-{sha}" so findAndDestroyInstancesBySHA matches it by suffix on
	// completion. Fall back to the push SHA on error (the periodic scan remains the backstop).
	cleanupSHA := sha
	if resolved, resErr := s.resolveBranchHeadSHA(s.Config.Org, repoName, branch); resErr == nil && resolved != "" {
		cleanupSHA = resolved
	} else if resErr != nil {
		logger.WithError(resErr).Warn("Failed to resolve branch HEAD SHA; keying cleanup on push SHA (periodic scan remains the backstop)")
	}

	// Store instances before dispatching so a fast-completing workflow_run event
	// doesn't race ahead and find nothing to clean up.
	key := fmt.Sprintf("%s-push-%s-%s", repoName, branch, cleanupSHA)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	err = s.triggerE2EWorkflowForPushEvent(repoName, instanceType, branch, sha, instances)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.Info("E2E workflow triggered successfully and instances tracked for cleanup")
}

// createMultipleE2EInstancesForPushEvent creates all platform instances in parallel.
// Results are returned in platforms[] order so index-based assignment is stable.
func (s *Server) createMultipleE2EInstancesForPushEvent(repoName, instanceType, branch string) ([]*E2EInstance, error) {
	var platforms []string
	if instanceType == "desktop" {
		platforms = []string{"linux", "macos", "windows"}
	} else {
		platforms = mobileE2EPlatforms
	}

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":          repoName,
		"instanceType":  instanceType,
		"platformCount": len(platforms),
	})

	serverVersion := s.serverVersionForPushEvent()
	sanitizedVersion := sanitizeForDNS(serverVersion)
	uid := e2eUniqueSuffix()

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
			inst, err := s.createCloudInstallation(ctx, name, serverVersion, username, password, instanceType, logger)
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
		s.destroyE2EInstances(instances, logger)
		return nil, firstErr
	}

	logger.WithField("instanceCount", len(instances)).Info("All E2E instances created successfully")
	return instances, nil
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

// triggerE2EWorkflowForPushEvent routes to the desktop or mobile dispatch function.
// Cleanup is driven by the workflow_run completed event matched on the commit SHA.
func (s *Server) triggerE2EWorkflowForPushEvent(repoName, instanceType, branch, sha string, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         repoName,
		"instanceType": instanceType,
		"branch":       branch,
		"sha":          sha,
	})

	repoOwner := s.Config.Org
	if repoOwner == "" {
		logger.Error("Organization not configured")
		return fmt.Errorf("organization not configured")
	}

	if instanceType == "desktop" {
		return s.triggerDesktopE2EWorkflowForPushEvent(repoOwner, repoName, branch, sha, instances)
	}

	return s.triggerMobileE2EWorkflowForPushEvent(repoOwner, repoName, branch, sha, instances)
}

// triggerDesktopE2EWorkflowForPushEvent dispatches the desktop E2E workflow.
func (s *Server) triggerDesktopE2EWorkflowForPushEvent(repoOwner, repoName, branch, sha string, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
	})

	instanceDetailsJSON, err := s.buildInstanceDetailsJSON(instances)
	if err != nil {
		logger.WithError(err).Error("Failed to build instance details JSON")
		return err
	}

	logger.WithField("instanceDetails", instanceDetailsJSON).Debug("Triggering desktop E2E workflow")

	// runType is always MASTER — only master/main pushes reach this path.
	return s.dispatchDesktopE2EWorkflow(repoOwner, repoName, branch, sha, instanceDetailsJSON, "MASTER")
}

// triggerMobileE2EWorkflowForPushEvent dispatches the mobile E2E workflow (e2e-detox-pr.yml).
func (s *Server) triggerMobileE2EWorkflowForPushEvent(repoOwner, repoName, branch, sha string, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
	})

	if len(instances) != len(mobileE2EPlatforms) {
		logger.Errorf("Mobile E2E requires %d instances, got %d", len(mobileE2EPlatforms), len(instances))
		return fmt.Errorf("mobile E2E requires %d instances", len(mobileE2EPlatforms))
	}

	logger.WithFields(logrus.Fields{
		"android_site_1_url": instances[0].URL,
		"android_site_2_url": instances[1].URL,
		"ios_site_1_url":     instances[2].URL,
		"ios_site_2_url":     instances[3].URL,
		"site_3_url":         instances[4].URL,
	}).Debug("Triggering mobile E2E workflow for push event")

	// handlePushEvent only routes master/main pushes here (release-branch push trigger was
	// removed), so runType is always MASTER for mobile push events.
	return s.dispatchMobileE2EWorkflow(
		repoOwner, repoName, branch, sha,
		instances[0].URL, instances[1].URL,
		instances[2].URL, instances[3].URL,
		instances[4].URL,
		"both", // push events always test both iOS and Android
		"MASTER",
	)
}

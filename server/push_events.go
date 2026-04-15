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

// handlePushEvent processes push events to trigger E2E tests on release branches or master/main
func (s *Server) handlePushEvent(event *github.PushEvent) {
	repoName := event.GetRepo().GetName()
	branchRef := event.GetRef() // Format: "refs/heads/branch-name"
	branch := extractBranchName(branchRef)

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
		"action": "push",
	})
	logger.Info("Push event received")

	// Detect if this is a release branch push
	if s.Config.E2EAutoTriggerOnRelease && s.isReleaseBranch(branch) {
		logger.WithField("type", "release_branch").Info("Release branch detected, triggering E2E tests")
		version := extractVersionFromReleaseBranch(branch, s.Config.E2EReleasePatternPrefix)
		go s.handlePushEventE2E(event, branch, version)
		return
	}

	// Detect if this is a master/main branch push
	if s.Config.E2EAutoTriggerOnMaster && (branch == "master" || branch == "main") {
		logger.WithField("type", "master_main").Info("Master/main branch detected, triggering E2E tests")
		go s.handlePushEventE2E(event, branch, "")
		return
	}

	logger.Debug("Push event does not match E2E trigger conditions")
}

// isReleaseBranch checks if a branch name is a release branch
func (s *Server) isReleaseBranch(branch string) bool {
	return strings.HasPrefix(branch, s.Config.E2EReleasePatternPrefix)
}

// extractBranchName extracts the branch name from the ref
// GitHub sends refs in the format "refs/heads/branch-name"
func extractBranchName(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) < 3 {
		return ref
	}
	return strings.Join(parts[2:], "/")
}

// extractVersionFromReleaseBranch extracts version from release branch name
// Example: "release-8.0" -> "8.0"
func extractVersionFromReleaseBranch(branch string, prefix string) string {
	if !strings.HasPrefix(branch, prefix) {
		return ""
	}
	return strings.TrimPrefix(branch, prefix)
}

// handlePushEventE2E orchestrates E2E testing for push events (release or master/main)
func (s *Server) handlePushEventE2E(event *github.PushEvent, branch string, version string) {
	repoName := event.GetRepo().GetName()
	commit := event.GetHeadCommit()
	sha := ""
	if commit != nil {
		sha = commit.GetID()
	}

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":    repoName,
		"branch":  branch,
		"version": version,
		"sha":     sha,
	})

	// Determine if this is a desktop or mobile repository
	isDesktop := strings.Contains(repoName, "desktop")
	isMobile := strings.Contains(repoName, "mobile")

	if !isDesktop && !isMobile {
		logger.Warn("Repository is neither desktop nor mobile, skipping E2E tests")
		return
	}

	// Create E2E instances for testing
	instanceType := "desktop"
	if isMobile {
		instanceType = "mobile"
	}

	logger.WithField("instanceType", instanceType).Info("Creating E2E instances for push event")

	// Create instances based on repo type
	instances, err := s.createMultipleE2EInstancesForPushEvent(repoName, instanceType, branch, version, sha)
	if err != nil {
		logger.WithError(err).Error("Failed to create E2E instances")
		return
	}

	if len(instances) == 0 {
		logger.Error("No instances created for E2E testing")
		return
	}

	logger.WithField("instanceCount", len(instances)).Info("E2E instances created successfully")

	// Track instances BEFORE dispatching so that a fast-completing workflow_run
	// completed event cannot race ahead of us and find nothing to clean up.
	key := fmt.Sprintf("%s-push-%s-%s", repoName, branch, sha)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	// Trigger the appropriate E2E workflow
	err = s.triggerE2EWorkflowForPushEvent(repoName, instanceType, branch, sha, instances)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		// Remove from tracking and destroy instances on dispatch failure
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
func (s *Server) createMultipleE2EInstancesForPushEvent(repoName, instanceType, branch, version, _ string) ([]*E2EInstance, error) {
	var platforms []string
	if instanceType == "desktop" {
		platforms = []string{"linux", "macos", "windows"}
	} else {
		platforms = []string{"site-1", "site-2", "site-3"}
	}

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":          repoName,
		"instanceType":  instanceType,
		"platformCount": len(platforms),
	})

	// Name format: {type}-{version}-{platform}-{hex6}
	serverVersion := s.resolveE2EServerVersion()
	if version != "" {
		serverVersion = version
	}
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

// getRunnerForPlatform returns the GitHub Actions runner for a given platform
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

// triggerE2EWorkflowForPushEvent triggers the E2E workflow for a push event
func (s *Server) triggerE2EWorkflowForPushEvent(repoName, instanceType, branch, sha string, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         repoName,
		"instanceType": instanceType,
		"branch":       branch,
		"sha":          sha,
	})

	// Determine repo owner
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

// triggerDesktopE2EWorkflowForPushEvent triggers the desktop E2E workflow
func (s *Server) triggerDesktopE2EWorkflowForPushEvent(repoOwner, repoName, branch, sha string, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
	})

	// Build instance details JSON for desktop workflow
	instanceDetailsJSON, err := s.buildInstanceDetailsJSON(instances)
	if err != nil {
		logger.WithError(err).Error("Failed to build instance details JSON")
		return err
	}

	logger.WithField("instanceDetails", instanceDetailsJSON).Debug("Triggering desktop E2E workflow")

	runType := "MASTER"
	if s.isReleaseBranch(branch) {
		runType = "RELEASE"
	}

	// Dispatch to the exact commit SHA so the workflow_run completed event carries the
	// same head_sha we stored in the tracking key ({repo}-push-{branch}-{sha}).
	// Using the branch ref risks a head_sha mismatch if another commit lands during
	// the ~30 min instance-creation window.
	return s.dispatchDesktopE2EWorkflow(repoOwner, repoName, sha, sha, instanceDetailsJSON, runType, false)
}

// triggerMobileE2EWorkflowForPushEvent triggers the mobile E2E workflow (e2e-detox-pr.yml)
func (s *Server) triggerMobileE2EWorkflowForPushEvent(repoOwner, repoName, branch, sha string, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
	})

	if len(instances) < 3 {
		logger.Errorf("Mobile E2E requires 3 instances, got %d", len(instances))
		return fmt.Errorf("mobile E2E requires 3 instances")
	}

	logger.WithFields(logrus.Fields{
		"site_1_url": instances[0].URL,
		"site_2_url": instances[1].URL,
		"site_3_url": instances[2].URL,
	}).Debug("Triggering mobile E2E workflow (e2e-detox-pr.yml) for push event")

	runType := "MASTER"
	if s.isReleaseBranch(branch) {
		runType = "RELEASE"
	}

	// Dispatch to the exact commit SHA (not branch) so the workflow_run completed event
	// carries the same head_sha stored in the tracking key ({repo}-push-{branch}-{sha}).
	return s.dispatchMobileE2EWorkflow(
		repoOwner, repoName, sha, sha,
		instances[0].URL, instances[1].URL, instances[2].URL,
		"both", // Push events (release/master) test both platforms
		runType,
	)
}


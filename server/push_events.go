// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"fmt"
	"strings"

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

	// Trigger the appropriate E2E workflow
	err = s.triggerE2EWorkflowForPushEvent(repoName, instanceType, branch, sha, instances)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		// Attempt cleanup on workflow trigger failure
		s.destroyE2EInstances(instances, logger)
		return
	}

	// Track instances for cleanup (keyed by repo-branch-sha to ensure uniqueness across multiple pushes)
	key := fmt.Sprintf("%s-push-%s-%s", repoName, branch, sha)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.Info("E2E workflow triggered successfully and instances tracked for cleanup")
}

// createMultipleE2EInstancesForPushEvent creates instances for push event E2E testing
func (s *Server) createMultipleE2EInstancesForPushEvent(repoName, instanceType, branch, version, _ string) ([]*E2EInstance, error) {
	var instances []*E2EInstance
	var platforms []string

	// For push events, always use all platforms
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

	sanitizedRepoName := strings.ToLower(repoName)
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, "_", "-")
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, ".", "-")

	sanitizedBranch := strings.ToLower(branch)
	sanitizedBranch = strings.ReplaceAll(sanitizedBranch, "/", "-")
	sanitizedBranch = strings.ReplaceAll(sanitizedBranch, "_", "-")
	sanitizedBranch = strings.ReplaceAll(sanitizedBranch, ".", "-")

	for _, platform := range platforms {
		suffix := fmt.Sprintf("-e2e-%s-%s", sanitizedBranch, platform)
		repoPrefix := sanitizedRepoName
		if maxLen := 63 - len(s.Config.DNSNameTestServer) - len(suffix); len(repoPrefix) > maxLen {
			if maxLen < 1 {
				maxLen = 1
			}
			repoPrefix = strings.TrimRight(repoPrefix[:maxLen], "-")
		}
		name := repoPrefix + suffix

		// Use version if provided, otherwise use server version from config
		serverVersion := s.Config.E2EServerVersion
		if version != "" {
			serverVersion = version
		}

		username := s.Config.E2EUsername

		// Get password from config or org-level secrets
		password := s.getE2EPassword(instanceType)

		instance, err := s.createCloudInstallation(name, serverVersion, username, password, logger)
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

	return s.dispatchDesktopE2EWorkflow(repoOwner, repoName, branch, sha, instanceDetailsJSON)
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

	// Use e2e-detox-pr.yml for ALL scenarios (PR, release, master)
	// Provide SITE_1/2/3_URL as individual inputs (not instance_details JSON)
	// For push events, always test both iOS and Android
	return s.dispatchMobileE2EWorkflow(
		repoOwner, repoName, branch, sha,
		instances[0].URL, instances[1].URL, instances[2].URL,
		"both", // Push events (release/master) test both platforms
	)
}

// handlePushEventE2ECleanup destroys E2E instances created for push events
// This should be called by a scheduled job or webhook when workflow completes
func (s *Server) handlePushEventE2ECleanup(repoName, branch string) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   repoName,
		"branch": branch,
		"type":   "e2e_cleanup_push",
	})
	logger.Info("Handling push event E2E cleanup")

	// Retrieve and remove instances from tracking.
	// Instances may be stored under a key including the SHA (e.g. "%s-push-%s-%s"),
	// so we collect and delete all matching keys for this repo/branch.
	s.e2eInstancesLock.Lock()
	var instances []*E2EInstance

	// Backwards-compatible: check the simple key without SHA.
	baseKey := fmt.Sprintf("%s-push-%s", repoName, branch)
	if v, ok := s.e2eInstances[baseKey]; ok {
		instances = append(instances, v...)
		delete(s.e2eInstances, baseKey)
	}

	// Also remove any SHA-suffixed keys for this repo/branch.
	prefixWithSHA := baseKey + "-"
	for k, v := range s.e2eInstances {
		if strings.HasPrefix(k, prefixWithSHA) {
			instances = append(instances, v...)
			delete(s.e2eInstances, k)
		}
	}
	s.e2eInstancesLock.Unlock()

	if len(instances) == 0 {
		logger.Warn("No E2E instances found for push event cleanup")
		return
	}

	logger.WithField("instances", len(instances)).Info("Destroying push event E2E instances")
	s.destroyE2EInstances(instances, logger)
}

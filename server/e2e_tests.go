// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v32/github"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	mattermostModel "github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/matterwick/internal/cloudtools"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

// E2EInstance represents a single E2E test server instance
// Note: Platform field has different meanings for desktop vs mobile:
//   - Desktop: Platform = OS runner (linux/macos/windows) where tests execute
//   - Mobile: Platform = instance identifier (site-1/site-2/site-3) for the test server
type E2EInstance struct {
	Name           string `json:"name"`
	Platform       string `json:"platform"` // Desktop: linux/macos/windows (OS runner), Mobile: site-1/site-2/site-3 (instance ID)
	Runner         string `json:"runner"`   // For desktop only: GitHub Actions runner label
	URL            string `json:"url"`
	InstallationID string `json:"installation_id"`
	ServerVersion  string `json:"server_version"`
}

// e2eUniqueSuffix returns an 8-character random hex suffix for instance name uniqueness.
// Uses cloudModel.NewID (crypto/rand-based UUID) truncated to 8 chars so that
// concurrent calls always produce distinct values regardless of clock resolution.
func e2eUniqueSuffix() string {
	return cloudModel.NewID()[:8]
}

// sanitizeForDNS lowercases and replaces non-DNS characters with hyphens.
func sanitizeForDNS(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "/", "-")
	return s
}

// e2eInstanceName builds a DNS-safe instance name and truncates if needed.
// parts are joined with "-". The total name + dnsSuffix must be <= 62.
func e2eInstanceName(dnsSuffix string, parts ...string) string {
	name := strings.Join(parts, "-")
	maxLen := 62 - len(dnsSuffix)
	if maxLen < 1 {
		maxLen = 1
	}
	if len(name) > maxLen {
		name = strings.TrimRight(name[:maxLen], "-")
	}
	return name
}

// handleE2ETestRequest is the main orchestrator for E2E test requests
func (s *Server) handleE2ETestRequest(pr *model.PullRequest, label string) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":  pr.RepoName,
		"pr":    pr.Number,
		"label": label,
		"type":  "e2e",
	})
	logger.Info("Handling E2E test request")

	// Determine instance type and platforms first — needed for both reuse lookup and creation.
	var instanceType string
	var platforms []string
	var testPlatform string // For mobile: which OS to test (ios/android/both). For desktop: unused (tests all OS platforms)

	if strings.Contains(pr.RepoName, "desktop") {
		instanceType = "desktop"
		platforms = []string{"linux", "macos", "windows"}
		testPlatform = "all"
	} else if strings.Contains(pr.RepoName, "mobile") {
		instanceType = "mobile"
		// Always create all 3 mobile instances (workflow expects SITE_1/2/3_URL).
		platforms = []string{"site-1", "site-2", "site-3"}
		testPlatform = s.extractPlatformFromLabel(label)
		logger.WithField("testPlatform", testPlatform).Info("Detected mobile test platform from label (ios/android/both)")
	} else {
		logger.Error("Unable to determine E2E instance type from repository name")
		return
	}

	key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)

	// Guard against duplicate webhook deliveries. The in-progress key includes
	// the test platform so that a second mobile label with a *different* platform
	// (e.g. E2E/Run-Android while E2E/Run-iOS is provisioning) is not incorrectly
	// dropped — it will reuse the in-flight instances once they are stored, or
	// create its own if they are not yet available.
	inProgressKey := fmt.Sprintf("%s-%s", key, testPlatform)
	s.e2eInProgressLock.Lock()
	if s.e2eInProgress[inProgressKey] {
		s.e2eInProgressLock.Unlock()
		logger.Warn("E2E instance creation already in progress for this PR and platform, skipping duplicate request")
		return
	}
	s.e2eInProgress[inProgressKey] = true
	s.e2eInProgressLock.Unlock()
	defer func() {
		s.e2eInProgressLock.Lock()
		delete(s.e2eInProgress, inProgressKey)
		s.e2eInProgressLock.Unlock()
	}()

	// 1. Reuse existing in-memory instances (servers stay alive between label toggles).
	s.e2eInstancesLock.Lock()
	existingInstances := s.e2eInstances[key]
	s.e2eInstancesLock.Unlock()

	if len(existingInstances) > 0 {
		logger.WithField("instances", len(existingInstances)).Info("Reusing existing in-memory E2E instances")
		s.cancelPRWorkflowRuns(pr, logger)
		s.wakeUpHibernatingInstances(existingInstances, logger)
		if err := s.triggerE2EWorkflow(pr, existingInstances, instanceType, testPlatform); err != nil {
			logger.WithError(err).Error("Failed to trigger E2E workflow with existing instances")
			s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
		}
		return
	}

	// 2. Check cloud API for instances that survived a matterwick restart.
	if cloudInstances, err := s.findExistingE2EInstancesInCloud(pr, instanceType, platforms); err == nil && len(cloudInstances) == len(platforms) {
		logger.WithField("instances", len(cloudInstances)).Info("Reusing existing cloud E2E instances")
		s.cancelPRWorkflowRuns(pr, logger)
		s.wakeUpHibernatingInstances(cloudInstances, logger)
		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = cloudInstances
		s.e2eInstancesLock.Unlock()
		if err := s.triggerE2EWorkflow(pr, cloudInstances, instanceType, testPlatform); err != nil {
			logger.WithError(err).Error("Failed to trigger E2E workflow with cloud instances")
			s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
		}
		return
	}

	// 3. No existing instances — create fresh ones.
	instances, err := s.createMultipleE2EInstances(pr, instanceType, platforms)
	if err != nil {
		logger.WithError(err).Error("Failed to create E2E instances")
		s.postE2EErrorComment(pr, fmt.Sprintf("Failed to create E2E test instances: %v", err))
		return
	}

	if len(instances) == 0 {
		logger.Error("No instances were created")
		s.postE2EErrorComment(pr, "Failed to create any E2E test instances")
		return
	}

	// Instance creation takes ~30 min. Check if the PR was closed during that window.
	// If so, destroy the freshly created instances — no further cleanup events will fire
	// for a closed PR, so storing them would leak them permanently.
	prInfo, _, prErr := newGithubClient(s.Config.GithubAccessToken).PullRequests.Get(
		context.Background(), pr.RepoOwner, pr.RepoName, pr.Number)
	if prErr != nil {
		logger.WithError(prErr).Warn("Failed to check PR state after instance creation; proceeding")
	} else if prInfo.GetState() == "closed" {
		logger.Warn("PR was closed during E2E instance creation; destroying instances without tracking")
		s.destroyE2EInstances(instances, logger)
		return
	}

	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.WithField("instances", len(instances)).Info("Successfully created E2E instances")

	if err = s.triggerE2EWorkflow(pr, instances, instanceType, testPlatform); err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
		// Remove from tracking before cleanup to avoid double-destroy on later cleanup.
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.Info("Successfully triggered E2E workflow")
}

// createMultipleE2EInstances creates all platform instances in parallel.
// Results are returned in the same order as platforms[] so that callers can rely on
// index-based platform assignment (e.g. instances[0] = site-1 for mobile).
func (s *Server) createMultipleE2EInstances(pr *model.PullRequest, instanceType string, platforms []string) ([]*E2EInstance, error) {
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no platforms specified")
	}

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":      pr.RepoName,
		"pr":        pr.Number,
		"type":      instanceType,
		"platforms": len(platforms),
	})

	version := s.resolveE2EServerVersion()
	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)
	// Name format: {type}-pr-{pr}-{platform}-{hex6}
	uid := e2eUniqueSuffix()

	// Shared cancellable context: the first goroutine to fail cancels the rest so they
	// exit their polling loop within one sleep interval (30s) instead of waiting up to 30min.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		instance *E2EInstance
		err      error
	}
	// Pre-allocate by index so each goroutine writes to its own slot — no mutex needed.
	results := make([]result, len(platforms))
	var wg sync.WaitGroup

	for i, platform := range platforms {
		wg.Add(1)
		go func(idx int, platform string) {
			defer wg.Done()
			instanceName := e2eInstanceName(
				s.Config.DNSNameTestServer,
				instanceType, fmt.Sprintf("pr-%d", pr.Number), platform, uid,
			)
			logger.WithField("instance", instanceName).Info("Creating E2E instance")
			inst, err := s.createCloudInstallation(ctx, instanceName, version, username, password, instanceType, logger)
			if err != nil {
				cancel() // signal sibling goroutines to stop waiting
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

	// Collect results in platforms[] order. On any error, destroy all that succeeded.
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

	return instances, nil
}

// createCloudInstallation creates a single installation via provisioner API.
// ctx is used to cancel the polling wait so that parallel callers can abort early when a
// sibling goroutine fails, instead of waiting up to 30 minutes per polling interval.
func (s *Server) createCloudInstallation(ctx context.Context, name, version, username, password, instanceType string, logger logrus.FieldLogger) (*E2EInstance, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("installation creation cancelled before request: %w", err)
	}

	// Create installation request
	envVars := cloudModel.EnvVarMap{
		"MM_SERVICESETTINGS_ENABLETUTORIAL":                cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_ENABLEONBOARDINGFLOW":          cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_ENABLEUSERTYPINGMESSAGES":      cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_SESSIONLENGTHMOBILEINHOURS":    cloudModel.EnvVar{Value: "5000"},
		"MM_SERVICESETTINGS_SESSIONCACHEINMINUTES":         cloudModel.EnvVar{Value: "180"},
		"MM_SERVICEENVIRONMENT":                            cloudModel.EnvVar{Value: "test"},
		"MM_RATELIMITSETTINGS_ENABLE":                         cloudModel.EnvVar{Value: "true"},
		"MM_RATELIMITSETTINGS_PERSEC":                         cloudModel.EnvVar{Value: "3000"},
		"MM_RATELIMITSETTINGS_MAXBURST":                       cloudModel.EnvVar{Value: "5000"},
		"MM_RATELIMITSETTINGS_MEMORYSTORESIZE":                cloudModel.EnvVar{Value: "10000"},
		"MM_RATELIMITSETTINGS_VARYBYREMOTEADDR":               cloudModel.EnvVar{Value: "false"},
		"MM_RATELIMITSETTINGS_VARYBYUSER":                     cloudModel.EnvVar{Value: "false"},
		"MM_TEAMSETTINGS_EXPERIMENTALENABLEAUTOMATICREPLIES":  cloudModel.EnvVar{Value: "true"},
	}

	installationRequest := &cloudModel.CreateInstallationRequest{
		OwnerID:     name,
		Version:     version,
		DNS:         fmt.Sprintf("%s.%s", name, s.Config.DNSNameTestServer),
		Size:        "miniSingleton",
		Affinity:    cloudModel.InstallationAffinityMultiTenant,
		Database:    cloudModel.InstallationDatabaseMultiTenantRDSPostgresPGBouncer,
		Filestore:   cloudModel.InstallationFilestoreBifrost,
		Annotations: []string{defaultMultiTenantAnnotation},
		PriorityEnv: envVars,
	}

	if len(s.Config.CloudGroupID) != 0 {
		installationRequest.GroupID = s.Config.CloudGroupID
	}

	// Create installation
	installation, err := s.CloudClient.CreateInstallation(installationRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to create installation: %w", err)
	}

	// cleanupCreatedInstallation is a best-effort cleanup helper used on all failure paths after
	// CreateInstallation succeeds. Without it, the cloud installation would be permanently
	// orphaned because it has not yet been added to the in-memory tracking map.
	// It deletes the installation, logs any deletion error, and returns cause unchanged.
	cleanupCreatedInstallation := func(cause error) error {
		if delErr := s.CloudClient.DeleteInstallation(installation.ID); delErr != nil {
			logger.WithError(delErr).WithField("installation_id", installation.ID).Error("Failed to clean up partially created installation")
		}
		return cause
	}

	logger.WithField("installation_id", installation.ID).Info("Installation created, waiting for stable state")

	// Wait for installation to be stable using polling with timeout
	timeout := time.Now().Add(30 * time.Minute)
	for {
		inst, err := s.CloudClient.GetInstallation(installation.ID, nil)
		if err != nil {
			return nil, cleanupCreatedInstallation(fmt.Errorf("failed to get installation status: %w", err))
		}

		if inst.State == cloudModel.InstallationStateStable || inst.State == cloudModel.InstallationStateHibernating {
			logger.WithField("state", inst.State).Info("Installation is stable")
			break
		}

		if time.Now().After(timeout) {
			return nil, cleanupCreatedInstallation(fmt.Errorf("timeout waiting for installation to become stable"))
		}

		// Context-aware sleep: wake immediately if a sibling goroutine failed and cancelled ctx.
		select {
		case <-ctx.Done():
			return nil, cleanupCreatedInstallation(fmt.Errorf("installation wait cancelled: %w", ctx.Err()))
		case <-time.After(30 * time.Second):
		}
	}

	// Check cancellation before the 10-minute initialization phase (DNS + ping + user setup).
	if err := ctx.Err(); err != nil {
		return nil, cleanupCreatedInstallation(fmt.Errorf("installation creation cancelled before initialization: %w", err))
	}

	// Initialize Mattermost server with provided credentials
	spinwickURL := fmt.Sprintf("https://%s", cloudtools.GetInstallationDNSFromDNSRecords(installation))
	err = s.initializeMattermostE2EServer(spinwickURL, username, password, logger)
	if err != nil {
		return nil, cleanupCreatedInstallation(fmt.Errorf("failed to initialize Mattermost server: %w", err))
	}

	return &E2EInstance{
		Name:           name,
		URL:            spinwickURL,
		InstallationID: installation.ID,
		ServerVersion:  version,
	}, nil
}

// getE2EPassword returns the password for E2E testing from config or org-level secrets
func (s *Server) getE2EPassword(instanceType string) string {
	var password string

	// Try config first, then fall back to environment variables
	password = s.Config.E2EPassword
	if password == "" {
		if instanceType == "mobile" {
			password = os.Getenv("MM_MOBILE_E2E_ADMIN_PASSWORD")
		} else {
			password = os.Getenv("MM_DESKTOP_E2E_USER_CREDENTIALS")
		}
	}

	if password == "" {
		s.Logger.Warnf("E2E password not configured for %s; instance creation may fail", instanceType)
	}
	return password
}

// initializeMattermostE2EServer initializes a Mattermost server with E2E credentials
func (s *Server) initializeMattermostE2EServer(spinwickURL, username, password string, logger logrus.FieldLogger) error {
	return s.setupE2EServerCredentials(spinwickURL, username, password, logger)
}

// createE2EDefaultTeam creates the 'ad-1' team and adds userID to it.
// Separated so it can be unit-tested without DNS/ping dependencies.
func createE2EDefaultTeam(client *mattermostModel.Client4, userID string, logger logrus.FieldLogger) error {
	team := &mattermostModel.Team{
		Name:        "ad-1",
		DisplayName: "ad-1",
		Type:        "O",
	}
	createdTeam, _, err := client.CreateTeam(team)
	if err != nil {
		return fmt.Errorf("failed to create E2E default team 'ad-1': %w", err)
	}
	_, _, err = client.AddTeamMember(createdTeam.Id, userID)
	if err != nil {
		return fmt.Errorf("failed to add admin user to E2E team 'ad-1': %w", err)
	}
	logger.Info("Created default team 'ad-1' and added admin user")
	return nil
}

// setupE2EServerCredentials sets up a Mattermost server with provided E2E credentials
func (s *Server) setupE2EServerCredentials(spinwickURL, username, password string, logger logrus.FieldLogger) error {
	logger.Info("Setting up E2E server with provided credentials")

	wait := 600
	logger.Infof("Waiting up to %d seconds for DNS to propagate", wait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	// Parse the URL to get the hostname
	mmHost, err := url.Parse(spinwickURL)
	if err != nil {
		return fmt.Errorf("failed to parse MM URL: %w", err)
	}

	// Check DNS
	if err := checkDNS(ctx, fmt.Sprintf("%s:443", mmHost.Host)); err != nil {
		return fmt.Errorf("timed out waiting for DNS to propagate: %w", err)
	}

	client := mattermostModel.NewAPIv4Client(spinwickURL)

	// Wait for Mattermost to be available
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	if err := checkMMPing(ctx, client, logger); err != nil {
		return fmt.Errorf("failed to get mattermost ping response: %w", err)
	}

	// Create the admin user with provided credentials
	adminUser := &mattermostModel.User{
		Username: username,
		Email:    fmt.Sprintf("%s@example.mattermost.com", username),
		Password: password,
	}
	_, _, err = client.CreateUser(adminUser)
	if err != nil {
		return fmt.Errorf("failed to create E2E admin user: %w", err)
	}

	// Login as admin and capture the user ID
	userLogged, _, err := client.Login(username, password)
	if err != nil {
		return fmt.Errorf("failed to log in as E2E admin user: %w", err)
	}

	// Create the default team 'ad-1' and add admin user to it
	if err := createE2EDefaultTeam(client, userLogged.Id, logger); err != nil {
		return err
	}

	logger.Info("E2E server setup complete")
	return nil
}

// triggerE2EWorkflow triggers the appropriate E2E workflow with instance details
// testPlatform parameter:
//   - For desktop: "all" (tests run on linux/macos/windows automatically)
//   - For mobile: "ios", "android", or "both" (determines which mobile OS to test)
func (s *Server) triggerE2EWorkflow(pr *model.PullRequest, instances []*E2EInstance, instanceType string, testPlatform string) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	if instanceType == "desktop" {
		// Desktop ignores testPlatform - always tests all OS platforms (linux/macos/windows)
		return s.triggerDesktopE2EWorkflow(ctx, client, pr, instances)
	} else if instanceType == "mobile" {
		// Mobile uses testPlatform to determine which mobile OS to test (ios/android/both)
		return s.triggerMobileE2EWorkflow(ctx, client, pr, instances, testPlatform)
	}

	return fmt.Errorf("unknown instance type: %s", instanceType)
}

// triggerDesktopE2EWorkflow triggers the desktop E2E workflow
func (s *Server) triggerDesktopE2EWorkflow(ctx context.Context, client *github.Client, pr *model.PullRequest, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
		"type": "desktop",
	})

	// Convert instances to JSON for workflow input using consistent schema
	instanceDetailsJSON, err := s.buildInstanceDetailsJSON(instances)
	if err != nil {
		return fmt.Errorf("failed to marshal instance details: %w", err)
	}

	// Use the github REST API to trigger the workflow_dispatch event
	body := map[string]interface{}{
		"ref": pr.Ref,
		"inputs": map[string]interface{}{
			"instance_details":  instanceDetailsJSON,
			"version_name":      pr.Ref,
			"MM_TEST_USER_NAME": s.Config.E2EUsername,
			"MM_SERVER_VERSION": instances[0].ServerVersion,
			"pr_number":         fmt.Sprintf("%d", pr.Number),
		},
	}

	logger.WithField("instances_json", string(instanceDetailsJSON)).Debug("Triggering desktop E2E workflow")

	req, err := client.NewRequest("POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-functional.yml/dispatches", pr.RepoOwner, pr.RepoName), body)
	if err != nil {
		return fmt.Errorf("failed to create workflow dispatch request: %w", err)
	}

	_, err = client.Do(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("failed to trigger desktop e2e workflow: %w", err)
	}

	logger.Info("Successfully triggered desktop E2E workflow")
	return nil
}

// triggerMobileE2EWorkflow triggers the mobile E2E workflow
// testPlatform specifies which mobile OS to test: "ios", "android", or "both"
func (s *Server) triggerMobileE2EWorkflow(ctx context.Context, client *github.Client, pr *model.PullRequest, instances []*E2EInstance, testPlatform string) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         pr.RepoName,
		"pr":           pr.Number,
		"type":         "mobile",
		"testPlatform": testPlatform, // ios/android/both
	})

	if len(instances) != 3 {
		return fmt.Errorf("mobile E2E requires exactly 3 instances, got %d", len(instances))
	}

	// Build workflow inputs dynamically based on the provided instances
	inputs := map[string]interface{}{
		"MOBILE_VERSION": pr.Sha,
		"PLATFORM":       testPlatform, // Workflow input: which mobile OS to test (ios/android/both)
		"pr_number":      fmt.Sprintf("%d", pr.Number),
	}
	for i, inst := range instances {
		// SITE_1_URL, SITE_2_URL, SITE_3_URL
		siteKey := fmt.Sprintf("SITE_%d_URL", i+1)
		inputs[siteKey] = inst.URL
	}

	// Use the github REST API to trigger the workflow_dispatch event
	body := map[string]interface{}{
		"ref":    pr.Ref,
		"inputs": inputs,
	}

	logger.WithField("workflow", "e2e-detox-pr.yml").Debug("Triggering mobile E2E workflow")

	req, err := client.NewRequest("POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-detox-pr.yml/dispatches", pr.RepoOwner, pr.RepoName), body)
	if err != nil {
		return fmt.Errorf("failed to create workflow dispatch request: %w", err)
	}

	_, err = client.Do(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("failed to trigger mobile e2e workflow: %w", err)
	}

	logger.Info("Successfully triggered mobile E2E workflow")
	return nil
}

// handleE2ECleanup destroys tracked E2E instances, then queries the cloud API by DNS pattern to catch orphans.
func (s *Server) handleE2ECleanup(pr *model.PullRequest) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
		"type": "e2e_cleanup",
	})
	logger.Info("Handling E2E cleanup request")

	// Fast path: in-memory map
	key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) > 0 {
		logger.WithField("instances", len(instances)).Info("Destroying tracked E2E instances")
		s.destroyE2EInstances(instances, logger)
	}

	// Fallback: catch orphans from restarts, map overwrites, or failed goroutines
	s.cleanupOrphanedE2EInstances(pr, logger)
}

// cleanupOrphanedE2EInstances queries the cloud API by DNS LIKE pattern and destroys any matches.
func (s *Server) cleanupOrphanedE2EInstances(pr *model.PullRequest, logger logrus.FieldLogger) {
	var instanceType string
	if strings.Contains(pr.RepoName, "desktop") {
		instanceType = "desktop"
	} else if strings.Contains(pr.RepoName, "mobile") {
		instanceType = "mobile"
	} else {
		logger.Debug("Skipping orphan E2E cleanup for non-E2E repo")
		return
	}

	dnsPattern := fmt.Sprintf("%s-pr-%d-%%", instanceType, pr.Number) // e.g. "mobile-pr-9587-%"

	installations, err := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{
		DNS:    dnsPattern,
		Paging: cloudModel.AllPagesNotDeleted(),
	})
	if err != nil {
		logger.WithError(err).Error("Failed to query cloud API for orphaned E2E instances")
		return
	}

	if len(installations) == 0 {
		logger.Debug("No orphaned E2E instances found via cloud API")
		return
	}

	logger.WithField("orphans", len(installations)).Warn("Found orphaned E2E instances via cloud API")
	for _, inst := range installations {
		// Skip instances already progressing through deletion to avoid redundant API calls.
		if inst.State == cloudModel.InstallationStateDeletionPendingRequested ||
			inst.State == cloudModel.InstallationStateDeletionPendingInProgress ||
			inst.State == cloudModel.InstallationStateDeletionPending ||
			inst.State == cloudModel.InstallationStateDeletionRequested ||
			inst.State == cloudModel.InstallationStateDeletionFailed ||
			inst.State == cloudModel.InstallationStateDeleted {
			logger.WithField("installation_id", inst.ID).Debug("Skipping E2E instance already in deletion state")
			continue
		}
		instLogger := logger.WithField("installation_id", inst.ID)
		instLogger.Info("Destroying orphaned E2E instance")
		if err := s.CloudClient.DeleteInstallation(inst.ID); err != nil {
			instLogger.WithError(err).Error("Failed to destroy orphaned E2E instance")
		}
	}
}

// e2eInstanceMaxAge returns the configured maximum age for non-PR E2E instances before
// they are considered orphaned and eligible for deletion by the periodic cleanup scan.
// Falls back to 3 hours when the config value is 0 (unset).
func (s *Server) e2eInstanceMaxAge() time.Duration {
	if s.Config.E2EInstanceMaxAge > 0 {
		return time.Duration(s.Config.E2EInstanceMaxAge) * time.Hour
	}
	return 3 * time.Hour
}

// PR instances (identified by "-pr-" in their OwnerID) are always skipped — handleE2ECleanup
// on PR close manages their lifecycle via cloud-API orphan scan.
func (s *Server) cleanupStaleNonPRE2EInstances() {
	maxAge := s.e2eInstanceMaxAge()
	logger := s.Logger.WithField("type", "periodic_e2e_cleanup")
	logger.WithField("max_age_hours", maxAge.Hours()).Info("Scanning for stale non-PR E2E instances")

	cutoffMs := time.Now().Add(-maxAge).UnixMilli()

	for _, instanceType := range []string{"desktop", "mobile"} {
		pattern := instanceType + "-%"
		installations, err := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{
			DNS:    pattern,
			Paging: cloudModel.AllPagesNotDeleted(),
		})
		if err != nil {
			logger.WithError(err).Errorf("Failed to query %s instances", instanceType)
			continue
		}

		for _, inst := range installations {
			// Guard against a nil embedded Installation — must come first, all other
			// field accesses (OwnerID, CreateAt, State, ID) are on the embedded struct.
			if inst.Installation == nil {
				logger.Warn("Skipping instance with nil Installation pointer in cleanup scan")
				continue
			}

			// PR instances have "-pr-" in their OwnerID (e.g. "mobile-pr-123-site-1-...").
			// Skip them — handleE2ECleanup on PR close manages their lifecycle.
			if strings.Contains(inst.OwnerID, "-pr-") {
				continue
			}

			// Skip instances younger than maxAge — a test may still be using them.
			if inst.CreateAt > cutoffMs {
				logger.WithFields(logrus.Fields{
					"installation_id": inst.ID,
					"owner_id":        inst.OwnerID,
				}).Debug("Skipping non-PR instance younger than max age (may still be in use)")
				continue
			}

			// Skip instances already progressing through deletion.
			if inst.State == cloudModel.InstallationStateDeletionPendingRequested ||
				inst.State == cloudModel.InstallationStateDeletionPendingInProgress ||
				inst.State == cloudModel.InstallationStateDeletionPending ||
				inst.State == cloudModel.InstallationStateDeletionRequested ||
				inst.State == cloudModel.InstallationStateDeletionFailed ||
				inst.State == cloudModel.InstallationStateDeleted {
				continue
			}

			instLogger := logger.WithFields(logrus.Fields{
				"installation_id": inst.ID,
				"owner_id":        inst.OwnerID,
				"state":           inst.State,
			})
			instLogger.Warn("Destroying stale non-PR E2E instance")
			if err := s.CloudClient.DeleteInstallation(inst.ID); err != nil {
				instLogger.WithError(err).Error("Failed to destroy stale non-PR E2E instance")
			}
		}
	}

	logger.Info("Non-PR E2E instance cleanup scan complete")
}

// resolveE2EServerVersion returns the Mattermost server version to use for E2E instances.
// If E2EServerVersion is "latest", it fetches the mattermost/mattermost GitHub releases,
// skips drafts, prerelease-flagged releases, and RC/beta/alpha tag-name patterns, then
// returns the first (newest) fully stable tag stripped of its "v" prefix
// (e.g. "v11.6.0" → "11.6.0") to match the Docker Hub tag format.
//
// The resolved version is cached in memory for 1 hour so that back-to-back provisioning
// calls (e.g. three parallel platform instances) share a single GitHub API round-trip.
// Falls back to "master" on any API error or when no stable release is found.
func (s *Server) resolveE2EServerVersion() string {
	if s.Config.E2EServerVersion != "latest" {
		return s.Config.E2EServerVersion
	}

	const cacheTTL = 1 * time.Hour

	// Return cached value if still fresh.
	s.e2eVersionCacheLock.Lock()
	if s.e2eVersionCache != "" && time.Since(s.e2eVersionCacheTime) < cacheTTL {
		cached := s.e2eVersionCache
		s.e2eVersionCacheLock.Unlock()
		s.Logger.WithField("version", cached).Debug("[resolveE2EServerVersion] Returning cached version")
		return cached
	}
	s.e2eVersionCacheLock.Unlock()

	// 10-second timeout prevents blocking instance-creation goroutines indefinitely
	// if the GitHub API is slow or unreachable.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := newGithubClient(s.Config.GithubAccessToken)

	// Redirect to a mock server when running tests (githubAPIBase is empty in production).
	if s.githubAPIBase != "" {
		if baseURL, parseErr := url.Parse(s.githubAPIBase); parseErr == nil {
			client.BaseURL = baseURL
		}
	}

	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}

	req, err := client.NewRequest("GET", "/repos/mattermost/mattermost/releases?per_page=20", nil)
	if err != nil {
		s.Logger.WithError(err).Warn("[resolveE2EServerVersion] Failed to build request, falling back to master")
		return "master"
	}
	if _, err = client.Do(ctx, req, &releases); err != nil {
		s.Logger.WithError(err).Warn("[resolveE2EServerVersion] Failed to fetch releases, falling back to master")
		return "master"
	}

	for _, r := range releases {
		// Skip drafts and GitHub's explicit prerelease flag first.
		if r.Draft || r.Prerelease {
			continue
		}
		// Also skip by tag-name pattern as a secondary guard for releases whose
		// prerelease flag may not be set correctly (e.g. some RC tags).
		lower := strings.ToLower(r.TagName)
		if strings.Contains(lower, "-rc") || strings.Contains(lower, "-beta") || strings.Contains(lower, "-alpha") {
			continue
		}
		// Strip "v" prefix to match Docker Hub tag format (e.g. "v11.6.0" → "11.6.0").
		version := strings.TrimPrefix(r.TagName, "v")
		s.Logger.WithField("version", version).Info("[resolveE2EServerVersion] Resolved latest Mattermost server version")

		s.e2eVersionCacheLock.Lock()
		s.e2eVersionCache = version
		s.e2eVersionCacheTime = time.Now()
		s.e2eVersionCacheLock.Unlock()

		return version
	}

	s.Logger.Warn("[resolveE2EServerVersion] No stable release found, falling back to master")
	return "master"
}

// destroyE2EInstances destroys all given E2E instances
func (s *Server) destroyE2EInstances(instances []*E2EInstance, logger logrus.FieldLogger) {
	for _, instance := range instances {
		logger := logger.WithField("instance_id", instance.InstallationID)
		logger.Info("Destroying E2E instance")

		err := s.CloudClient.DeleteInstallation(instance.InstallationID)
		if err != nil {
			logger.WithError(err).Error("Failed to destroy E2E instance")
			continue
		}

		logger.Info("Successfully destroyed E2E instance")
	}
}

// findExistingE2EInstancesInCloud queries the cloud API for E2E instances that match a PR and
// reconstructs E2EInstance objects for reuse. Returns an error if the count doesn't match
// the expected number of platforms (indicating a partial or fully absent set).
func (s *Server) findExistingE2EInstancesInCloud(pr *model.PullRequest, instanceType string, platforms []string) ([]*E2EInstance, error) {
	dnsPattern := fmt.Sprintf("%s-pr-%d-%%", instanceType, pr.Number)

	installations, err := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{
		DNS:    dnsPattern,
		Paging: cloudModel.AllPagesNotDeleted(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query cloud API for existing E2E instances: %w", err)
	}

	// Only reuse instances that are in a stable, usable state.
	var reusable []*cloudModel.InstallationDTO
	for _, inst := range installations {
		if inst.State == cloudModel.InstallationStateStable ||
			inst.State == cloudModel.InstallationStateHibernating {
			reusable = append(reusable, inst)
		}
	}

	// Parse the platform token from each OwnerID.
	// OwnerID format: {type}-{version}-{platform}-{uid} where uid is 8 hex chars.
	// Strip the trailing "-{uid}" (9 chars) then check which expected platform is a suffix.
	platformByInst := make(map[string]*cloudModel.InstallationDTO, len(reusable)) // platform → inst
	for _, inst := range reusable {
		ownerID := inst.OwnerID
		var matched string
		for _, p := range platforms {
			// OwnerID without uid ends with "-{platform}"; uid is 9 chars ("-" + 8 hex).
			withoutUID := ownerID
			if len(ownerID) > 9 {
				withoutUID = ownerID[:len(ownerID)-9]
			}
			if strings.HasSuffix(withoutUID, "-"+p) {
				matched = p
				break
			}
		}
		if matched == "" {
			return nil, fmt.Errorf("could not determine platform for instance %q", ownerID)
		}
		if _, dup := platformByInst[matched]; dup {
			return nil, fmt.Errorf("duplicate platform %q found among cloud instances", matched)
		}
		platformByInst[matched] = inst
	}

	// Validate that every expected platform is present.
	for _, p := range platforms {
		if _, ok := platformByInst[p]; !ok {
			return nil, fmt.Errorf("expected platform %q not found among cloud instances (found %d, want %d)", p, len(reusable), len(platforms))
		}
	}

	// Build result in platforms[] order so index-based assignment is stable for callers.
	result := make([]*E2EInstance, len(platforms))
	for i, platform := range platforms {
		inst := platformByInst[platform]
		e2eInst := &E2EInstance{
			Name:           inst.OwnerID,
			Platform:       platform,
			URL:            fmt.Sprintf("https://%s.%s", inst.OwnerID, s.Config.DNSNameTestServer),
			InstallationID: inst.ID,
			ServerVersion:  inst.Version,
		}
		if instanceType == "desktop" {
			e2eInst.Runner = getRunnerForPlatform(platform)
		}
		result[i] = e2eInst
	}
	return result, nil
}

// wakeUpHibernatingInstances checks each instance and wakes any that are hibernating,
// waiting up to 10 minutes for stable state. Logs warnings on failure and proceeds.
func (s *Server) wakeUpHibernatingInstances(instances []*E2EInstance, logger logrus.FieldLogger) {
	for _, inst := range instances {
		installation, err := s.CloudClient.GetInstallation(inst.InstallationID, nil)
		if err != nil {
			logger.WithError(err).WithField("installation_id", inst.InstallationID).Warn("Failed to check installation state before wake-up")
			continue
		}
		if installation.State != cloudModel.InstallationStateHibernating {
			continue
		}
		logger.WithField("installation_id", inst.InstallationID).Info("Waking up hibernating E2E instance")
		if _, err := s.CloudClient.WakeupInstallation(inst.InstallationID, nil); err != nil {
			logger.WithError(err).WithField("installation_id", inst.InstallationID).Warn("Failed to wake up hibernating E2E instance")
			continue
		}
		timeout := time.Now().Add(10 * time.Minute)
		for {
			updated, err := s.CloudClient.GetInstallation(inst.InstallationID, nil)
			if err != nil {
				logger.WithError(err).WithField("installation_id", inst.InstallationID).Warn("Error polling installation state during wake-up")
				break
			}
			if updated.State == cloudModel.InstallationStateStable {
				logger.WithField("installation_id", inst.InstallationID).Info("Hibernating E2E instance is now stable")
				break
			}
			if time.Now().After(timeout) {
				logger.WithField("installation_id", inst.InstallationID).Warn("Timeout waiting for E2E instance to wake up")
				break
			}
			time.Sleep(15 * time.Second)
		}
	}
}

// postE2EStartedComment posts a comment when E2E tests start
func (s *Server) postE2EStartedComment(pr *model.PullRequest, instances []*E2EInstance) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	var platformsList string
	for _, inst := range instances {
		platformsList += fmt.Sprintf("- %s: %s\n", inst.Platform, inst.URL)
	}

	comment := fmt.Sprintf(`## E2E Test Servers Created

The following test servers have been created and are ready for E2E testing:

%s

Tests will run against these servers. Please monitor the workflow run for progress.`, platformsList)

	_, _, err := client.Issues.CreateComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, &github.IssueComment{
		Body: &comment,
	})

	if err != nil {
		s.Logger.WithError(err).Error("Failed to post E2E started comment")
	}
}

// postE2EErrorComment posts an error comment
func (s *Server) postE2EErrorComment(pr *model.PullRequest, errorMsg string) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	comment := fmt.Sprintf("❌ E2E Test Setup Failed\n\n%s", errorMsg)

	_, _, err := client.Issues.CreateComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, &github.IssueComment{
		Body: &comment,
	})

	if err != nil {
		s.Logger.WithError(err).Error("Failed to post E2E error comment")
	}
}

// buildInstanceDetailsJSON builds the instance details JSON for desktop workflows
func (s *Server) buildInstanceDetailsJSON(instances []*E2EInstance) (string, error) {
	type instanceDetail struct {
		Platform       string `json:"platform"`
		Runner         string `json:"runner"`
		URL            string `json:"url"`
		InstallationID string `json:"installation-id"`
		ServerVersion  string `json:"server_version"`
	}

	details := make([]instanceDetail, len(instances))
	for i, inst := range instances {
		details[i] = instanceDetail{
			Platform:       inst.Platform,
			Runner:         inst.Runner,
			URL:            inst.URL,
			InstallationID: inst.InstallationID,
			ServerVersion:  inst.ServerVersion,
		}
	}

	jsonBytes, err := json.Marshal(details)
	if err != nil {
		return "", err
	}

	return string(jsonBytes), nil
}

// dispatchDesktopE2EWorkflow triggers the desktop E2E workflow via GitHub Actions API.
// trackingKey is the s.e2eInstances map key for this run; when non-empty it is passed
// as the "mw_tracking_key" workflow input so the workflow_run completed handler can do
// a direct key lookup instead of fragile SHA suffix matching.
func (s *Server) dispatchDesktopE2EWorkflow(repoOwner, repoName, ref, sha, instanceDetailsJSON, runType, trackingKey string, nightly bool) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"ref":  ref,
	})

	// Determine the server version to use for the workflow.
	// Default to the configured E2E server version, but prefer the actual
	// provisioned version from the instance details when available.
	serverVersion := s.resolveE2EServerVersion()
	if instanceDetailsJSON != "" {
		var instances []struct {
			ServerVersion string `json:"server_version"`
		}
		if err := json.Unmarshal([]byte(instanceDetailsJSON), &instances); err == nil {
			if len(instances) > 0 && instances[0].ServerVersion != "" {
				serverVersion = instances[0].ServerVersion
			}
		}
	}

	// Build the workflow dispatch request
	workflowInputs := map[string]interface{}{
		"instance_details":  instanceDetailsJSON,
		"version_name":      ref,
		"MM_TEST_USER_NAME": s.Config.E2EUsername,
		"MM_SERVER_VERSION": serverVersion,
		"run_type":          runType,
		"nightly":           fmt.Sprintf("%t", nightly),
	}
	if trackingKey != "" {
		workflowInputs["mw_tracking_key"] = trackingKey
	}

	// Use REST API to trigger workflow dispatch (v32 go-github compatibility)
	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-functional.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    ref,
			"inputs": workflowInputs,
		})
	if err != nil {
		logger.WithError(err).Error("Failed to create workflow dispatch request")
		return err
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger desktop E2E workflow")
		return err
	}

	if resp.StatusCode != 204 {
		logger.WithField("status_code", resp.StatusCode).Error("Unexpected response from workflow dispatch")
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Info("Desktop E2E workflow dispatched successfully")
	return nil
}

// dispatchMobileE2EWorkflow triggers the mobile E2E workflow via GitHub Actions API.
// trackingKey is the s.e2eInstances map key for this run; when non-empty it is passed
// as the "mw_tracking_key" workflow input so the workflow_run completed handler can do
// a direct key lookup instead of fragile SHA suffix matching.
func (s *Server) dispatchMobileE2EWorkflow(repoOwner, repoName, ref, sha, site1URL, site2URL, site3URL, platform, runType, trackingKey string) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"ref":  ref,
	})

	// Build the workflow dispatch request
	workflowInputs := map[string]interface{}{
		"SITE_1_URL":     site1URL,
		"SITE_2_URL":     site2URL,
		"SITE_3_URL":     site3URL,
		"MOBILE_VERSION": sha,
		"PLATFORM":       platform,
		"run_type":       runType,
	}
	if trackingKey != "" {
		workflowInputs["mw_tracking_key"] = trackingKey
	}

	// Use REST API to trigger workflow dispatch (v32 go-github compatibility)
	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-detox-pr.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    ref,
			"inputs": workflowInputs,
		})
	if err != nil {
		logger.WithError(err).Error("Failed to create workflow dispatch request")
		return err
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger mobile E2E workflow")
		return err
	}

	if resp.StatusCode != 204 {
		logger.WithField("status_code", resp.StatusCode).Error("Unexpected response from workflow dispatch")
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Info("Mobile E2E workflow dispatched successfully")
	return nil
}

// cancelPRWorkflowRuns cancels any in-progress E2E workflow runs for a PR
// This is called when a new E2E run is triggered for the same PR
func (s *Server) cancelPRWorkflowRuns(pr *model.PullRequest, logger logrus.FieldLogger) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	logger = logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
	})

	logger.Info("Attempting to cancel in-progress E2E workflow runs")

	// Determine which workflow file to cancel based on repository type
	var workflowFile string
	if strings.Contains(pr.RepoName, "desktop") {
		workflowFile = "e2e-functional.yml"
	} else if strings.Contains(pr.RepoName, "mobile") {
		workflowFile = "e2e-detox-pr.yml"
	} else {
		logger.Warn("Unable to determine workflow file for repository")
		return
	}

	// List workflow runs for this workflow file
	// GitHub API v32 limitation: we need to use REST API directly
	listURL := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?status=in_progress&event=workflow_dispatch",
		pr.RepoOwner, pr.RepoName, workflowFile)

	req, err := client.NewRequest("GET", listURL, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to create workflow runs list request")
		return
	}

	var workflowRuns struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			HeadBranch string `json:"head_branch"`
			Status     string `json:"status"`
		} `json:"workflow_runs"`
	}

	_, err = client.Do(ctx, req, &workflowRuns)
	if err != nil {
		logger.WithError(err).Error("Failed to list workflow runs")
		return
	}

	// Cancel workflow runs that match this PR's branch
	cancelCount := 0
	for _, run := range workflowRuns.WorkflowRuns {
		// Check if this run is for the PR's branch
		if run.HeadBranch == pr.Ref && run.Status == "in_progress" {
			cancelURL := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/cancel",
				pr.RepoOwner, pr.RepoName, run.ID)

			cancelReq, err := client.NewRequest("POST", cancelURL, nil)
			if err != nil {
				logger.WithError(err).WithField("run_id", run.ID).Error("Failed to create cancel request")
				continue
			}

			_, err = client.Do(ctx, cancelReq, nil)
			if err != nil {
				logger.WithError(err).WithField("run_id", run.ID).Error("Failed to cancel workflow run")
				continue
			}

			logger.WithField("run_id", run.ID).Info("Cancelled workflow run")
			cancelCount++
		}
	}

	if cancelCount > 0 {
		logger.WithField("cancelled_runs", cancelCount).Info("Successfully cancelled workflow runs")
	} else {
		logger.Debug("No in-progress workflow runs found to cancel")
	}
}

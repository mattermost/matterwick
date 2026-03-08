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

// handleE2ETestRequest is the main orchestrator for E2E test requests
func (s *Server) handleE2ETestRequest(pr *model.PullRequest, label string) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":  pr.RepoName,
		"pr":    pr.Number,
		"label": label,
		"type":  "e2e",
	})
	logger.Info("Handling E2E test request")

	// Check if there's already an E2E run in progress for this PR
	// If yes, cancel it before starting a new one
	key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
	s.e2eInstancesLock.Lock()
	existingInstances, hasExisting := s.e2eInstances[key]
	if hasExisting {
		logger.WithField("existingInstances", len(existingInstances)).Info("Found existing E2E run, canceling it")
		// Remove from tracking immediately to prevent race conditions
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()

		// Destroy old instances in background
		go s.destroyE2EInstances(existingInstances, logger)

		// Also attempt to cancel the GitHub workflow run
		go s.cancelPRWorkflowRuns(pr, logger)
	} else {
		s.e2eInstancesLock.Unlock()
	}

	// Determine instance type based on repository
	var instanceType string
	var platforms []string
	var testPlatform string // For mobile: which OS to test (ios/android/both). For desktop: unused (tests all OS platforms)

	if strings.Contains(pr.RepoName, "desktop") {
		instanceType = "desktop"
		// Desktop: platforms = OS runners (linux/macos/windows)
		// Desktop tests run on all OS platforms automatically
		platforms = []string{"linux", "macos", "windows"}
		testPlatform = "all" // Desktop always tests all OS platforms (linux/macos/windows)
	} else if strings.Contains(pr.RepoName, "mobile") {
		instanceType = "mobile"
		// Mobile: platforms = server instances (site-1/site-2/site-3)
		// Always create all 3 mobile instances (workflow expects SITE_1/2/3_URL).
		platforms = []string{"site-1", "site-2", "site-3"}
		// Mobile: testPlatform = which mobile OS to test (ios/android/both)
		testPlatform = s.extractPlatformFromLabel(label)
		logger.WithField("testPlatform", testPlatform).Info("Detected mobile test platform from label (ios/android/both)")
	} else {
		logger.Error("Unable to determine E2E instance type from repository name")
		s.postE2EErrorComment(pr, "Unable to determine E2E instance type. Only desktop and mobile repos are supported.")
		return
	}

	// Create multiple instances
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

	// Store instances for later cleanup (reuse key variable from above)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.WithField("instances", len(instances)).Info("Successfully created E2E instances")

	// Trigger the appropriate workflow
	err = s.triggerE2EWorkflow(pr, instances, instanceType, testPlatform) // Pass testPlatform: "all" for desktop, "ios"/"android"/"both" for mobile
	if err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
		// Remove instances from tracking map before cleanup to avoid double-destroy on later cleanup.
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		// Attempt cleanup on workflow trigger failure
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.Info("Successfully triggered E2E workflow")
	s.postE2EStartedComment(pr, instances)
}

// createMultipleE2EInstances creates multiple instances for E2E testing
func (s *Server) createMultipleE2EInstances(pr *model.PullRequest, instanceType string, platforms []string) ([]*E2EInstance, error) {
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no platforms specified")
	}

	var instances []*E2EInstance

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":      pr.RepoName,
		"pr":        pr.Number,
		"type":      instanceType,
		"platforms": len(platforms),
	})

	// Create username and password for this E2E test set
	var username, password string
	username = s.Config.E2EUsername

	// Get password from environment or generate one
	password = s.getE2EPassword(instanceType)

	sanitizedRepo := strings.ToLower(pr.RepoName)
	sanitizedRepo = strings.ReplaceAll(sanitizedRepo, "_", "-")
	sanitizedRepo = strings.ReplaceAll(sanitizedRepo, ".", "-")

	for _, platform := range platforms {
		suffix := fmt.Sprintf("-e2e-%d-%s", pr.Number, platform)
		maxRepoLen := 63 - len(s.Config.DNSNameTestServer) - len(suffix)
		if maxRepoLen < 1 {
			maxRepoLen = 1
		}
		repo := sanitizedRepo
		if len(repo) > maxRepoLen {
			repo = strings.TrimRight(repo[:maxRepoLen], "-")
		}
		instanceName := repo + suffix

		logger.WithField("instance", instanceName).Info("Creating E2E instance")

		// Create the installation
		instance, err := s.createCloudInstallation(instanceName, s.Config.E2EServerVersion, username, password, instanceType, logger)
		if err != nil {
			logger.WithError(err).Error("Failed to create cloud installation")
			// Cleanup already created instances on failure
			s.destroyE2EInstances(instances, logger)
			return nil, err
		}

		instance.Platform = platform
		if instanceType == "desktop" {
			// Assign appropriate runner for each platform
			instance.Runner = getRunnerForPlatform(platform)
		}

		instances = append(instances, instance)
		logger.WithField("instance", instanceName).Info("Successfully created E2E instance")
	}

	return instances, nil
}

// createCloudInstallation creates a single installation via provisioner API
func (s *Server) createCloudInstallation(name, version, username, password, instanceType string, logger logrus.FieldLogger) (*E2EInstance, error) {
	// Create installation request
	envVars := cloudModel.EnvVarMap{
		"MM_SERVICESETTINGS_ENABLETUTORIAL":       cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_ENABLEONBOARDINGFLOW": cloudModel.EnvVar{Value: "false"},
		"MM_SERVICEENVIRONMENT":                   cloudModel.EnvVar{Value: "test"},
	}

	// Enable automatic replies for mobile E2E tests
	if instanceType == "mobile" {
		envVars["MM_TEAMSETTINGS_EXPERIMENTALENABLEAUTOMATICREPLIES"] = cloudModel.EnvVar{Value: "true"}
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

	logger.WithField("installation_id", installation.ID).Info("Installation created, waiting for stable state")

	// Wait for installation to be stable using polling with timeout
	timeout := time.Now().Add(30 * time.Minute)
	for {
		inst, err := s.CloudClient.GetInstallation(installation.ID, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get installation status: %w", err)
		}

		if inst.State == cloudModel.InstallationStateStable || inst.State == cloudModel.InstallationStateHibernating {
			logger.WithField("state", inst.State).Info("Installation is stable")
			break
		}

		if time.Now().After(timeout) {
			return nil, fmt.Errorf("timeout waiting for installation to become stable")
		}

		time.Sleep(30 * time.Second)
	}

	// Initialize Mattermost server with provided credentials
	spinwickURL := fmt.Sprintf("https://%s", cloudtools.GetInstallationDNSFromDNSRecords(installation))
	err = s.initializeMattermostE2EServer(spinwickURL, username, password, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Mattermost server: %w", err)
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
			"MM_SERVER_VERSION": s.Config.E2EServerVersion,
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

// handleE2ECleanup destroys all E2E instances for a PR
func (s *Server) handleE2ECleanup(pr *model.PullRequest) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
		"type": "e2e_cleanup",
	})
	logger.Info("Handling E2E cleanup request")

	// Retrieve and remove instances from tracking
	key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) == 0 {
		logger.Warn("No E2E instances found for cleanup")
		return
	}

	logger.WithField("instances", len(instances)).Info("Destroying E2E instances")
	s.destroyE2EInstances(instances, logger)
}

// destroyE2EInstances destroys all given E2E instances
func (s *Server) destroyE2EInstances(instances []*E2EInstance, logger logrus.FieldLogger) {
	if s.CloudClient == nil {
		logger.Warn("CloudClient is nil; skipping instance destruction")
		return
	}
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

// dispatchDesktopE2EWorkflow triggers the desktop E2E workflow via GitHub Actions API
func (s *Server) dispatchDesktopE2EWorkflow(repoOwner, repoName, ref, sha, instanceDetailsJSON, runType string, nightly bool) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"ref":  ref,
	})

	// Determine the server version to use for the workflow.
	// Default to the configured E2E server version, but prefer the actual
	// provisioned version from the instance details when available.
	serverVersion := s.Config.E2EServerVersion
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

// dispatchMobileE2EWorkflow triggers the mobile E2E workflow via GitHub Actions API
func (s *Server) dispatchMobileE2EWorkflow(repoOwner, repoName, ref, sha, site1URL, site2URL, site3URL, platform, runType string) error {
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

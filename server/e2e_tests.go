// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
type E2EInstance struct {
	Name           string `json:"name"`
	Platform       string `json:"platform"` // For desktop: linux/macos/windows, For mobile: site-1/site-2/site-3
	Runner         string `json:"runner"`   // For desktop only
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

	// Determine instance type based on repository
	var instanceType string
	var platforms []string

	if strings.Contains(pr.RepoName, "desktop") {
		instanceType = "desktop"
		platforms = []string{"linux", "macos", "windows"}
	} else if strings.Contains(pr.RepoName, "mobile") {
		instanceType = "mobile"
		// Detect platform(s) from label
		if label == s.Config.E2EMobileIOSLabel {
			platforms = []string{"site-1"} // iOS only
		} else if label == s.Config.E2EMobileAndroidLabel {
			platforms = []string{"site-2", "site-3"} // Android (needs 2 sites)
		} else {
			// E2E/Run or default - create all 3 instances
			platforms = []string{"site-1", "site-2", "site-3"}
		}
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

	// Store instances for later cleanup
	key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.WithField("instances", len(instances)).Info("Successfully created E2E instances")

	// Trigger the appropriate workflow
	err = s.triggerE2EWorkflow(pr, instances, instanceType)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
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
	if instanceType == "desktop" {
		username = s.Config.E2EDesktopUsername
	} else {
		username = s.Config.E2EMobileUsername
	}

	// Get password from environment or generate one
	password = s.getE2EPassword(instanceType)

	for _, platform := range platforms {
		// Create unique name per instance
		instanceName := fmt.Sprintf("e2e-%s-%s-%d-%s", instanceType, pr.RepoName, pr.Number, platform)
		instanceName = strings.ToLower(instanceName)
		instanceName = strings.ReplaceAll(instanceName, "_", "-")
		instanceName = strings.ReplaceAll(instanceName, ".", "-")

		logger.WithField("instance", instanceName).Info("Creating E2E instance")

		// Create the installation
		instance, err := s.createCloudInstallation(instanceName, s.Config.E2EServerVersion, username, password, logger)
		if err != nil {
			logger.WithError(err).Error("Failed to create cloud installation")
			// Cleanup already created instances on failure
			s.destroyE2EInstances(instances, logger)
			return nil, err
		}

		instance.Platform = platform
		if instanceType == "desktop" {
			// Assign appropriate runner for each platform
			switch platform {
			case "linux":
				instance.Runner = "ubuntu-latest"
			case "macos":
				instance.Runner = "macos-latest"
			case "windows":
				instance.Runner = "windows-2022"
			}
		}

		instances = append(instances, instance)
		logger.WithField("instance", instanceName).Info("Successfully created E2E instance")
	}

	return instances, nil
}

// createCloudInstallation creates a single installation via provisioner API
func (s *Server) createCloudInstallation(name, version, username, password string, logger logrus.FieldLogger) (*E2EInstance, error) {
	// Create installation request
	installationRequest := &cloudModel.CreateInstallationRequest{
		OwnerID:     name,
		Version:     version,
		DNS:         fmt.Sprintf("%s.%s", name, s.Config.DNSNameTestServer),
		Size:        "miniSingleton",
		Affinity:    cloudModel.InstallationAffinityMultiTenant,
		Database:    cloudModel.InstallationDatabaseMultiTenantRDSPostgresPGBouncer,
		Filestore:   cloudModel.InstallationFilestoreBifrost,
		Annotations: []string{defaultMultiTenantAnnotation},
		PriorityEnv: cloudModel.EnvVarMap{
			"MM_SERVICESETTINGS_ENABLETUTORIAL":       cloudModel.EnvVar{Value: "false"},
			"MM_SERVICESETTINGS_ENABLEONBOARDINGFLOW": cloudModel.EnvVar{Value: "false"},
			"MM_SERVICEENVIRONMENT":                   cloudModel.EnvVar{Value: "test"},
		},
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

// getE2EPassword returns the password for E2E testing from environment or generates one
func (s *Server) getE2EPassword(instanceType string) string {
	// In a production scenario, these would come from org secrets
	// For now, we'll use a placeholder that can be overridden
	// The actual secrets are referenced in workflow inputs
	if instanceType == "desktop" {
		return "TestPassword@1"
	} else if instanceType == "mobile" {
		return "TestPassword@1"
	}
	return "TestPassword@1"
}

// initializeMattermostE2EServer initializes a Mattermost server with E2E credentials
func (s *Server) initializeMattermostE2EServer(spinwickURL, username, password string, logger logrus.FieldLogger) error {
	return s.setupE2EServerCredentials(spinwickURL, username, password, logger)
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

	// Login as admin
	_, _, err = client.Login(username, password)
	if err != nil {
		return fmt.Errorf("failed to log in as E2E admin user: %w", err)
	}

	logger.Info("E2E server setup complete")
	return nil
}

// triggerE2EWorkflow triggers the appropriate E2E workflow with instance details
func (s *Server) triggerE2EWorkflow(pr *model.PullRequest, instances []*E2EInstance, instanceType string) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	if instanceType == "desktop" {
		return s.triggerDesktopE2EWorkflow(ctx, client, pr, instances)
	} else if instanceType == "mobile" {
		return s.triggerMobileE2EWorkflow(ctx, client, pr, instances)
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

	// Convert instances to JSON for workflow input
	instanceDetailsJSON, err := json.Marshal(instances)
	if err != nil {
		return fmt.Errorf("failed to marshal instance details: %w", err)
	}

	// Use the github REST API to trigger the workflow_dispatch event
	body := map[string]interface{}{
		"ref": pr.Ref,
		"inputs": map[string]interface{}{
			"instance_details":  string(instanceDetailsJSON),
			"version_name":      pr.Ref,
			"MM_TEST_USER_NAME": s.Config.E2EDesktopUsername,
			"MM_SERVER_VERSION": s.Config.E2EServerVersion,
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
func (s *Server) triggerMobileE2EWorkflow(ctx context.Context, client *github.Client, pr *model.PullRequest, instances []*E2EInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
		"type": "mobile",
	})

	if len(instances) != 3 {
		return fmt.Errorf("mobile E2E requires exactly 3 instances, got %d", len(instances))
	}

	// Use the github REST API to trigger the workflow_dispatch event
	body := map[string]interface{}{
		"ref": pr.Ref,
		"inputs": map[string]interface{}{
			"SITE_1_URL":     instances[0].URL,
			"SITE_2_URL":     instances[1].URL,
			"SITE_3_URL":     instances[2].URL,
			"MOBILE_VERSION": pr.Sha,
			"PLATFORM":       "both",
		},
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
	}

	details := make([]instanceDetail, len(instances))
	for i, inst := range instances {
		details[i] = instanceDetail{
			Platform:       inst.Platform,
			Runner:         inst.Runner,
			URL:            inst.URL,
			InstallationID: inst.InstallationID,
		}
	}

	jsonBytes, err := json.Marshal(details)
	if err != nil {
		return "", err
	}

	return string(jsonBytes), nil
}

// dispatchDesktopE2EWorkflow triggers the desktop E2E workflow via GitHub Actions API
func (s *Server) dispatchDesktopE2EWorkflow(repoOwner, repoName, ref, sha, instanceDetailsJSON string) error {
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
		"MM_TEST_USER_NAME": s.Config.E2EDesktopUsername,
		"MM_SERVER_VERSION": serverVersion,
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
func (s *Server) dispatchMobileE2EWorkflow(repoOwner, repoName, ref, sha, site1URL, site2URL, site3URL string) error {
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
		"PLATFORM":       "both",
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

// dispatchDesktopCMTWorkflow triggers the desktop CMT workflow
func (s *Server) dispatchDesktopCMTWorkflow(repoOwner, repoName string, prNumber int, instanceDetailsJSON string) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"pr":   prNumber,
	})

	// Determine the server version to use for the workflow.
	// For CMT, instances may have different server versions, so we use the first
	// instance's version as the base version for the workflow.
	serverVersion := s.Config.E2EServerVersion
	if instanceDetailsJSON != "" {
		var instances []struct {
			ServerVersion string `json:"server-version"`
		}
		if err := json.Unmarshal([]byte(instanceDetailsJSON), &instances); err == nil {
			if len(instances) > 0 && instances[0].ServerVersion != "" {
				serverVersion = instances[0].ServerVersion
			}
		}
	}

	// Build the workflow dispatch request for CMT
	workflowInputs := map[string]interface{}{
		"instance_details":  instanceDetailsJSON,
		"MM_TEST_USER_NAME": s.Config.E2EDesktopUsername,
		"MM_SERVER_VERSION": serverVersion,
		"cmt_mode":          "true",
	}

	// Use REST API to trigger workflow dispatch
	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-functional.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    "HEAD",
			"inputs": workflowInputs,
		})
	if err != nil {
		logger.WithError(err).Error("Failed to create CMT workflow dispatch request")
		return err
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger desktop CMT workflow")
		return err
	}

	if resp.StatusCode != 204 {
		logger.WithField("status_code", resp.StatusCode).Error("Unexpected response from CMT workflow dispatch")
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Info("Desktop CMT workflow dispatched successfully")
	return nil
}

// dispatchMobileCMTWorkflow triggers the mobile CMT workflow
func (s *Server) dispatchMobileCMTWorkflow(repoOwner, repoName string, prNumber int, instances interface{}) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"pr":   prNumber,
	})

	// Build the workflow dispatch request for mobile CMT
	workflowInputs := map[string]interface{}{
		"cmt_mode": "true",
	}

	// Use REST API to trigger workflow dispatch
	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-detox-pr.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    "HEAD",
			"inputs": workflowInputs,
		})
	if err != nil {
		logger.WithError(err).Error("Failed to create mobile CMT workflow dispatch request")
		return err
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger mobile CMT workflow")
		return err
	}

	if resp.StatusCode != 204 {
		logger.WithField("status_code", resp.StatusCode).Error("Unexpected response from mobile CMT workflow dispatch")
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Info("Mobile CMT workflow dispatched successfully")
	return nil
}

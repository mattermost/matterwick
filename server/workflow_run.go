// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// WorkflowRunWebhookPayload represents the workflow_run webhook payload with inputs
type WorkflowRunWebhookPayload struct {
	Action      string                                `json:"action"`
	WorkflowRun WorkflowRunWithInputs                 `json:"workflow_run"`
	Repository  map[string]interface{}                `json:"repository"`
	Workflow    map[string]interface{}                `json:"workflow"`
}

// WorkflowRunWithInputs extends WorkflowRun with inputs field
type WorkflowRunWithInputs struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	HeadBranch      string `json:"head_branch"`
	HeadSHA         string `json:"head_sha"`
	Inputs          map[string]string `json:"inputs"`
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

// handleWorkflowRunEventWithInputs handles GitHub workflow_run events for CMT
func (s *Server) handleWorkflowRunEventWithInputs(payload *WorkflowRunWebhookPayload) {
	if payload.Action != "requested" {
		return
	}

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

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":     repoName,
		"owner":    owner,
		"workflow": workflowName,
		"action":   payload.Action,
	})

	// Check if this is a CMT-enabled workflow
	if !strings.Contains(workflowName, "cmt") && !strings.Contains(workflowName, "CMT") {
		logger.Debug("Workflow is not CMT-related, skipping")
		return
	}

	logger.Info("Processing CMT workflow_run event")

	// Extract server_versions from workflow inputs
	serverVersionsStr, ok := payload.WorkflowRun.Inputs["server_versions"]
	if !ok || serverVersionsStr == "" {
		logger.Error("No server_versions found in workflow inputs")
		return
	}

	serverVersions := parseServerVersionsFromString(serverVersionsStr)
	if len(serverVersions) == 0 {
		logger.Error("Failed to parse server versions from workflow input")
		return
	}

	logger.WithField("serverVersions", serverVersions).Info("Extracted server versions from workflow inputs")

	// Determine instance type based on repository
	var instanceType string
	if strings.Contains(repoName, "desktop") {
		instanceType = "desktop"
	} else if strings.Contains(repoName, "mobile") {
		instanceType = "mobile"
	} else {
		logger.Warn("Repository is neither desktop nor mobile, skipping CMT")
		return
	}

	logger.WithField("instanceType", instanceType).Info("Triggering CMT with server versions")

	// Handle CMT with server versions
	go s.handleCMTWithServerVersions(owner, repoName, instanceType, headBranch, serverVersions, logger)
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

// handleCMTWithServerVersions orchestrates CMT testing for multiple server versions
func (s *Server) handleCMTWithServerVersions(repoOwner, repoName, instanceType, branch string, serverVersions []string, logger logrus.FieldLogger) {
	logger = logger.WithFields(logrus.Fields{
		"serverVersionCount": len(serverVersions),
		"branch":             branch,
	})

	logger.Info("Starting CMT with server versions")

	// For each server version, create instances
	var allInstances []*E2EInstance

	for _, version := range serverVersions {
		version = strings.TrimSpace(version)
		if version == "" {
			continue
		}

		logger.WithField("version", version).Info("Creating instances for server version")

		// Create instances for this version
		instances, err := s.createCMTInstancesForVersion(repoName, instanceType, version)
		if err != nil {
			logger.WithError(err).Errorf("Failed to create instances for version %s", version)
			continue
		}

		if len(instances) > 0 {
			allInstances = append(allInstances, instances...)
			logger.WithFields(logrus.Fields{
				"version":        version,
				"instanceCount":  len(instances),
				"totalInstances": len(allInstances),
			}).Info("Instances created for version")
		}
	}

	if len(allInstances) == 0 {
		logger.Error("No instances created for any version")
		return
	}

	logger.WithField("totalInstances", len(allInstances)).Info("All instances created")

	// Trigger appropriate workflow based on instance type
	if instanceType == "desktop" {
		err := s.triggerCMTDesktopWorkflowWithInstances(repoOwner, repoName, branch, allInstances, logger)
		if err != nil {
			logger.WithError(err).Error("Failed to trigger desktop CMT workflow")
			s.destroyE2EInstances(allInstances, logger)
		}
	} else if instanceType == "mobile" {
		err := s.triggerCMTMobileWorkflowWithVersions(repoOwner, repoName, branch, serverVersions, allInstances, logger)
		if err != nil {
			logger.WithError(err).Error("Failed to trigger mobile CMT workflows")
			s.destroyE2EInstances(allInstances, logger)
		}
	}
}

// createCMTInstancesForVersion creates 3 instances (one per platform) for a given server version
func (s *Server) createCMTInstancesForVersion(repoName, instanceType, version string) ([]*E2EInstance, error) {
	var instances []*E2EInstance
	var platforms []string

	if instanceType == "desktop" {
		platforms = []string{"linux", "macos", "windows"}
	} else {
		platforms = []string{"site-1", "site-2", "site-3"}
	}

	sanitizedRepoName := strings.ToLower(repoName)
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, "_", "-")
	sanitizedRepoName = strings.ReplaceAll(sanitizedRepoName, ".", "-")

	sanitizedVersion := strings.ReplaceAll(version, ".", "-")

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":    repoName,
		"type":    instanceType,
		"version": version,
	})

	// Get credentials
	username := s.Config.E2EDesktopUsername
	if instanceType == "mobile" {
		username = s.Config.E2EMobileUsername
	}
	password := s.getE2EPassword(instanceType)

	// Add timestamp to ensure unique instance names across multiple CMT runs
	timestamp := time.Now().Unix()

	for i, platform := range platforms {
		name := fmt.Sprintf("%s-cmt-%s-%s-%d-%d", sanitizedRepoName, sanitizedVersion, platform, i+1, timestamp)

		instance, err := s.createCloudInstallation(name, version, username, password, logger)
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

	logger.WithField("instanceCount", len(instances)).Info("Instances created for version")
	return instances, nil
}

// triggerCMTDesktopWorkflowWithInstances triggers desktop CMT workflow with all instances
func (s *Server) triggerCMTDesktopWorkflowWithInstances(repoOwner, repoName, branch string, instances []*E2EInstance, logger logrus.FieldLogger) error {
	logger = logger.WithFields(logrus.Fields{
		"instanceCount": len(instances),
		"branch":        branch,
	})

	// Build instance details JSON
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

	instanceDetailsJSON, err := marshalToJSON(details)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal instance details")
		return err
	}

	logger.Debug("Triggering desktop CMT workflow with instance details")

	// Dispatch workflow with all instances
	return s.dispatchDesktopCMTWorkflowForServerVersions(repoOwner, repoName, branch, instanceDetailsJSON, logger)
}

// triggerCMTMobileWorkflowWithVersions triggers mobile CMT workflow once per version
func (s *Server) triggerCMTMobileWorkflowWithVersions(repoOwner, repoName, branch string, serverVersions []string, instances []*E2EInstance, logger logrus.FieldLogger) error {
	logger.Info("Triggering mobile CMT workflows for each server version")

	instancesPerVersion := 3
	createdVersionIdx := 0

	for _, version := range serverVersions {
		version = strings.TrimSpace(version)
		if version == "" {
			continue
		}

		// Get the 3 instances for this version
		startIdx := createdVersionIdx * instancesPerVersion
		endIdx := startIdx + instancesPerVersion

		if endIdx > len(instances) {
			logger.WithField("version", version).Warn("Not enough instances for this version")
			// No more complete instance sets are available for subsequent versions.
			break
		}

		versionInstances := instances[startIdx:endIdx]
		createdVersionIdx++

		versionLogger := logger.WithFields(logrus.Fields{
			"version":       version,
			"instanceCount": len(versionInstances),
		})

		versionLogger.Debug("Dispatching mobile CMT workflow for version")

		// Dispatch workflow for this version
		err := s.dispatchMobileCMTWorkflowForVersion(repoOwner, repoName, branch, version, versionInstances, versionLogger)
		if err != nil {
			versionLogger.WithError(err).Error("Failed to dispatch mobile CMT workflow for version")
			// Continue with next version instead of failing completely
			continue
		}
	}

	return nil
}

// extractServerVersionFromInstanceDetails attempts to derive a server version from the instance details JSON.
// It returns an empty string if no version can be determined.
func extractServerVersionFromInstanceDetails(instanceDetailsJSON string, logger logrus.FieldLogger) string {
	if strings.TrimSpace(instanceDetailsJSON) == "" {
		return ""
	}

	type instance struct {
		MMServerVersion string `json:"MM_SERVER_VERSION"`
		ServerVersion   string `json:"server_version"`
		Version         string `json:"version"`
	}

	var instances []instance
	if err := json.Unmarshal([]byte(instanceDetailsJSON), &instances); err != nil {
		logger.WithError(err).Debug("Failed to parse instance details JSON for server version")
		return ""
	}

	if len(instances) == 0 {
		return ""
	}

	inst := instances[0]
	switch {
	case strings.TrimSpace(inst.MMServerVersion) != "":
		return strings.TrimSpace(inst.MMServerVersion)
	case strings.TrimSpace(inst.ServerVersion) != "":
		return strings.TrimSpace(inst.ServerVersion)
	case strings.TrimSpace(inst.Version) != "":
		return strings.TrimSpace(inst.Version)
	default:
		return ""
	}
}

// dispatchDesktopCMTWorkflowForServerVersions dispatches desktop CMT workflow via workflow_dispatch
func (s *Server) dispatchDesktopCMTWorkflowForServerVersions(repoOwner, repoName, branch, instanceDetailsJSON string, logger logrus.FieldLogger) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	serverVersion := extractServerVersionFromInstanceDetails(instanceDetailsJSON, logger)

	workflowInputs := map[string]interface{}{
		"instance_details":  instanceDetailsJSON,
		"MM_TEST_USER_NAME": s.Config.E2EDesktopUsername,
		"cmt_mode":          "true",
		"MM_SERVER_VERSION": serverVersion,
	}

	logger.WithField("branch", branch).Debug("Dispatching desktop CMT workflow")

	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-functional.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    branch,
			"inputs": workflowInputs,
		})
	if err != nil {
		logger.WithError(err).Error("Failed to create workflow dispatch request")
		return err
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to dispatch desktop CMT workflow")
		return err
	}

	if resp.StatusCode != 204 {
		logger.WithField("status_code", resp.StatusCode).Error("Unexpected response from workflow dispatch")
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Info("Desktop CMT workflow dispatched successfully")
	return nil
}

// dispatchMobileCMTWorkflowForVersion dispatches mobile CMT workflow for a specific version
func (s *Server) dispatchMobileCMTWorkflowForVersion(repoOwner, repoName, branch, version string, instances []*E2EInstance, logger logrus.FieldLogger) error {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	if len(instances) != 3 {
		return fmt.Errorf("expected 3 instances for mobile CMT, got %d", len(instances))
	}

	workflowInputs := map[string]interface{}{
		"SITE_1_URL":     instances[0].URL,
		"SITE_2_URL":     instances[1].URL,
		"SITE_3_URL":     instances[2].URL,
		"cmt_mode":       "true",
		"server_version": version,
	}

	logger.WithFields(logrus.Fields{
		"branch": branch,
		"site_1": instances[0].URL,
		"site_2": instances[1].URL,
		"site_3": instances[2].URL,
	}).Debug("Dispatching mobile CMT workflow for version")

	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e-detox-pr.yml/dispatches", repoOwner, repoName),
		map[string]interface{}{
			"ref":    branch,
			"inputs": workflowInputs,
		})
	if err != nil {
		logger.WithError(err).Error("Failed to create mobile CMT workflow dispatch request")
		return err
	}

	resp, err := client.Do(ctx, req, nil)
	if err != nil {
		logger.WithError(err).Error("Failed to dispatch mobile CMT workflow")
		return err
	}

	if resp.StatusCode != 204 {
		logger.WithField("status_code", resp.StatusCode).Error("Unexpected response from mobile CMT workflow dispatch")
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Info("Mobile CMT workflow dispatched successfully for version")
	return nil
}

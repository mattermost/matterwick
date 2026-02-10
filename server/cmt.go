// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-github/v32/github"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

// CMTMatrix represents a compatibility matrix for testing
type CMTMatrix struct {
	ServerVersions []string `json:"serverVersions"`
	ClientVersions []string `json:"clientVersions"`
}

// CMTInstance represents a single instance in the CMT matrix
type CMTInstance struct {
	*E2EInstance
	ServerVersion string
	ClientVersion string
	MatrixIndex   int // M*N position in matrix
}

// handleCMTTestRequest orchestrates CMT (Compatibility Matrix Testing) for a PR
func (s *Server) handleCMTTestRequest(pr *model.PullRequest) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   pr.RepoName,
		"pr":     pr.Number,
		"action": "cmt_request",
	})
	logger.Info("CMT test request received")

	// Detect if this is a desktop or mobile repository
	isDesktop := strings.Contains(pr.RepoName, "desktop")
	isMobile := strings.Contains(pr.RepoName, "mobile")

	if !isDesktop && !isMobile {
		logger.Warn("Repository is neither desktop nor mobile, skipping CMT")
		return
	}

	instanceType := "desktop"
	if isMobile {
		instanceType = "mobile"
	}

	// Parse CMT matrix from PR description or use default
	matrix, err := s.parseCMTMatrixFromPR(pr)
	if err != nil {
		logger.WithError(err).Error("Failed to parse CMT matrix")
		s.postCMTErrorComment(pr, fmt.Sprintf("Failed to parse CMT matrix: %v", err))
		return
	}

	if matrix == nil {
		logger.Warn("No CMT matrix defined, using default matrix")
		matrix = s.getDefaultCMTMatrix()
	}

	logger.WithFields(logrus.Fields{
		"serverVersionCount": len(matrix.ServerVersions),
		"clientVersionCount": len(matrix.ClientVersions),
		"totalInstances":     len(matrix.ServerVersions) * len(matrix.ClientVersions),
	}).Info("CMT matrix parsed successfully")

	// Create CMT instances
	instances, err := s.createCMTInstances(pr.RepoName, instanceType, matrix)
	if err != nil {
		logger.WithError(err).Error("Failed to create CMT instances")
		s.postCMTErrorComment(pr, fmt.Sprintf("Failed to create CMT instances: %v", err))
		return
	}

	if len(instances) == 0 {
		logger.Error("No CMT instances created")
		s.postCMTErrorComment(pr, "Failed to create CMT instances")
		return
	}

	logger.WithField("instanceCount", len(instances)).Info("CMT instances created successfully")

	// Store instances for cleanup
	key := fmt.Sprintf("%s-cmt-pr-%d", pr.RepoName, pr.Number)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = convertCMTToE2EInstances(instances)
	s.e2eInstancesLock.Unlock()

	// Trigger CMT workflows
	err = s.triggerCMTWorkflows(pr, instanceType, instances)
	if err != nil {
		logger.WithError(err).Error("Failed to trigger CMT workflows")
		s.postCMTErrorComment(pr, fmt.Sprintf("Failed to trigger CMT workflows: %v", err))
		// Attempt cleanup on failure
		s.destroyE2EInstances(convertCMTToE2EInstances(instances), logger)
		return
	}

	logger.Info("CMT workflows triggered successfully")
	s.postCMTStartedComment(pr, instances, matrix)
}

// handleCMTCleanup destroys CMT instances when label is removed
func (s *Server) handleCMTCleanup(pr *model.PullRequest) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
	})

	key := fmt.Sprintf("%s-cmt-pr-%d", pr.RepoName, pr.Number)
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) == 0 {
		logger.Debug("No CMT instances to clean up")
		return
	}

	logger.WithField("instanceCount", len(instances)).Info("Cleaning up CMT instances")
	s.destroyE2EInstances(instances, logger)
}

// parseCMTMatrixFromPR parses CMT matrix from PR description
func (s *Server) parseCMTMatrixFromPR(pr *model.PullRequest) (*CMTMatrix, error) {
	// For now, return nil to use default matrix
	// In production, this would parse from PR description or config file
	// Example format in PR description:
	// ```cmt
	// serverVersions: ["8.0", "8.1", "9.0"]
	// clientVersions: ["1.0", "1.1", "1.2"]
	// ```
	return nil, nil
}

// getDefaultCMTMatrix returns a default CMT matrix for testing
func (s *Server) getDefaultCMTMatrix() *CMTMatrix {
	return &CMTMatrix{
		ServerVersions: []string{"8.0", "9.0"},
		ClientVersions: []string{"1.0", "1.1"},
	}
}

// createCMTInstances creates all instances for the CMT matrix
func (s *Server) createCMTInstances(repoName, instanceType string, matrix *CMTMatrix) ([]*CMTInstance, error) {
	var instances []*CMTInstance
	instanceIndex := 0

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         repoName,
		"instanceType": instanceType,
		"matrixSize":   fmt.Sprintf("%dx%d", len(matrix.ServerVersions), len(matrix.ClientVersions)),
	})

	for sIdx, serverVersion := range matrix.ServerVersions {
		for cIdx, clientVersion := range matrix.ClientVersions {
			instanceIndex++
			name := fmt.Sprintf("%s-cmt-%s-%s-%d", repoName, serverVersion, clientVersion, instanceIndex)

			logger.WithFields(logrus.Fields{
				"serverVersion": serverVersion,
				"clientVersion": clientVersion,
				"index":         instanceIndex,
			}).Debug("Creating CMT instance")

			// Create the base E2E instance
			e2eInstance, err := s.createCloudInstallation(name, serverVersion, s.Config.E2EDesktopUsername, "tempPassword", logger)
			if err != nil {
				logger.WithError(err).Errorf("Failed to create CMT instance at position (%d,%d)", sIdx, cIdx)
				// Cleanup already created instances on failure
				s.destroyCMTInstances(instances, logger)
				return nil, err
			}

			// Wrap in CMT instance with version info
			cmtInstance := &CMTInstance{
				E2EInstance:   e2eInstance,
				ServerVersion: serverVersion,
				ClientVersion: clientVersion,
				MatrixIndex:   instanceIndex,
			}

			// Set platform for desktop
			if instanceType == "desktop" {
				// Use different platforms across instances for better parallelization
				platforms := []string{"linux", "macos", "windows"}
				platform := platforms[instanceIndex%len(platforms)]
				cmtInstance.Platform = platform
				cmtInstance.Runner = getRunnerForPlatform(platform)
			} else {
				// For mobile, use site naming
				siteNames := []string{"site-1", "site-2", "site-3"}
				cmtInstance.Platform = siteNames[instanceIndex%len(siteNames)]
			}

			instances = append(instances, cmtInstance)
		}
	}

	logger.WithField("totalInstancesCreated", len(instances)).Info("All CMT instances created successfully")
	return instances, nil
}

// triggerCMTWorkflows triggers appropriate CMT workflows
func (s *Server) triggerCMTWorkflows(pr *model.PullRequest, instanceType string, instances []*CMTInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         pr.RepoName,
		"instanceType": instanceType,
		"pr":           pr.Number,
	})

	if len(instances) == 0 {
		return fmt.Errorf("no instances to test")
	}

	repoOwner := s.Config.Org
	if repoOwner == "" {
		logger.Error("Organization not configured")
		return fmt.Errorf("organization not configured")
	}

	if instanceType == "desktop" {
		return s.triggerDesktopCMTWorkflow(repoOwner, pr.RepoName, pr.Number, instances)
	}

	return s.triggerMobileCMTWorkflow(repoOwner, pr.RepoName, pr.Number, instances)
}

// triggerDesktopCMTWorkflow triggers desktop CMT workflow
func (s *Server) triggerDesktopCMTWorkflow(repoOwner, repoName string, prNumber int, instances []*CMTInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"pr":   prNumber,
	})

	// Build instance details for desktop CMT
	type instanceDetail struct {
		Platform       string `json:"platform"`
		Runner         string `json:"runner"`
		URL            string `json:"url"`
		InstallationID string `json:"installation-id"`
		ServerVersion  string `json:"server-version"`
		ClientVersion  string `json:"client-version"`
		MatrixIndex    int    `json:"matrix-index"`
	}

	details := make([]instanceDetail, len(instances))
	for i, inst := range instances {
		details[i] = instanceDetail{
			Platform:       inst.Platform,
			Runner:         inst.Runner,
			URL:            inst.URL,
			InstallationID: inst.InstallationID,
			ServerVersion:  inst.ServerVersion,
			ClientVersion:  inst.ClientVersion,
			MatrixIndex:    inst.MatrixIndex,
		}
	}

	instanceDetailsJSON, err := marshalToJSON(details)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal instance details")
		return err
	}

	logger.WithField("instanceCount", len(instances)).Debug("Triggering desktop CMT workflow")

	// For now, we'll use the standard e2e-functional.yml with CMT suffix
	// In production, this might trigger a dedicated cmt-e2e-functional.yml workflow
	return s.dispatchDesktopCMTWorkflow(repoOwner, repoName, prNumber, instanceDetailsJSON)
}

// triggerMobileCMTWorkflow triggers mobile CMT workflow
func (s *Server) triggerMobileCMTWorkflow(repoOwner, repoName string, prNumber int, instances []*CMTInstance) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": repoName,
		"pr":   prNumber,
	})

	if len(instances) < 1 {
		logger.Error("Not enough instances for mobile CMT")
		return fmt.Errorf("not enough instances for mobile CMT")
	}

	logger.WithField("instanceCount", len(instances)).Debug("Triggering mobile CMT workflow")

	// For mobile CMT, we dispatch the workflow with a reference to all instances
	// The workflow will iterate through and test each combination
	return s.dispatchMobileCMTWorkflow(repoOwner, repoName, prNumber, instances)
}

// destroyCMTInstances destroys all CMT instances
func (s *Server) destroyCMTInstances(instances []*CMTInstance, logger logrus.FieldLogger) {
	e2eInstances := convertCMTToE2EInstances(instances)
	s.destroyE2EInstances(e2eInstances, logger)
}

// convertCMTToE2EInstances converts CMT instances to E2E instances for cleanup
func convertCMTToE2EInstances(cmtInstances []*CMTInstance) []*E2EInstance {
	instances := make([]*E2EInstance, len(cmtInstances))
	for i, cmt := range cmtInstances {
		instances[i] = cmt.E2EInstance
	}
	return instances
}

// marshalToJSON marshals data to JSON string
func marshalToJSON(data interface{}) (string, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

// postCMTStartedComment posts a comment indicating CMT has started
func (s *Server) postCMTStartedComment(pr *model.PullRequest, instances []*CMTInstance, matrix *CMTMatrix) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	// Build instance list for display
	instanceList := fmt.Sprintf("Testing %d server versions × %d client versions = %d total combinations\n\n",
		len(matrix.ServerVersions), len(matrix.ClientVersions), len(instances))

	instanceList += "**Server Versions**: " + strings.Join(matrix.ServerVersions, ", ") + "\n"
	instanceList += "**Client Versions**: " + strings.Join(matrix.ClientVersions, ", ") + "\n\n"

	instanceList += "**Instances**:\n"
	for _, inst := range instances {
		instanceList += fmt.Sprintf("- Server %s + Client %s: %s\n",
			inst.ServerVersion, inst.ClientVersion, inst.URL)
	}

	comment := fmt.Sprintf(`🔄 CMT (Compatibility Matrix) E2E Testing Started

%s

Tests will run against all combinations. Please monitor the workflow run for progress.`, instanceList)

	_, _, err := client.Issues.CreateComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, &github.IssueComment{
		Body: &comment,
	})

	if err != nil {
		s.Logger.WithError(err).Error("Failed to post CMT started comment")
	}
}

// postCMTErrorComment posts an error comment for CMT failures
func (s *Server) postCMTErrorComment(pr *model.PullRequest, errorMsg string) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	comment := fmt.Sprintf("❌ CMT Test Setup Failed\n\n%s", errorMsg)

	_, _, err := client.Issues.CreateComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, &github.IssueComment{
		Body: &comment,
	})

	if err != nil {
		s.Logger.WithError(err).Error("Failed to post CMT error comment")
	}
}

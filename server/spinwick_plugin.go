// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/internal/spinwick"
	"github.com/mattermost/matterwick/model"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	defaultPluginImage   = "mattermostdevelopment/mattermost-enterprise-edition"
	defaultPluginVersion = "master"
	pluginRepoPrefix     = "mattermost-plugin-"
	pluginS3Bucket       = "mattermost-plugin-pr-builds"
	pluginS3Region       = "us-east-1" // Adjust if needed
)

// isPluginRepository checks if the repository is a plugin repository
func (s *Server) isPluginRepository(repoName string) bool {
	return strings.HasPrefix(repoName, pluginRepoPrefix)
}

// createPluginSpinWick creates a SpinWick for a plugin repository
func (s *Server) createPluginSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	// Use shortened name for DNS (e.g., "playbooks" instead of "mattermost-plugin-playbooks")
	pluginID := strings.TrimPrefix(pr.RepoName, pluginRepoPrefix)
	spinwick := model.NewSpinwick(pluginID, pr.Number, s.Config.DNSNameTestServer)
	// But use full repo name for the ownerID to maintain consistency with existing installations
	spinwick.RepoName = pr.RepoName
	spinwick.RepeatableID = fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
	ownerID := spinwick.RepeatableID

	// Check if installation already exists
	installation, err := s.checkExistingInstallation(ownerID, logger)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}
	if installation != nil {
		return request.WithInstallationID(installation.ID).
			WithError(errors.Errorf("Already found an installation belonging to %s", ownerID)).
			IntentionalAbort()
	}

	logger.Info("No plugin SpinWick found for this PR. Creating a new one.")

	// Create the Mattermost installation
	cloudClient := s.CloudClient
	installationRequest := s.createInstallationRequest(
		ownerID,
		defaultPluginVersion,
		defaultPluginImage,
		spinwick.DNS(s.Config.DNSNameTestServer),
		"miniSingleton",
		false, // no license needed for plugins
		nil,   // no env vars for plugins
	)

	installation, err = cloudClient.CreateInstallation(installationRequest)
	if err != nil {
		return request.WithError(
			errors.Wrap(err, "unable to make the installation creation request to the provisioning server")).
			ShouldReportError()
	}
	request.InstallationID = installation.ID
	logger = logger.WithField("installation_id", request.InstallationID)

	// Wait for installation to become stable and initialize
	wait := 1200
	logger.Infof("Waiting %d seconds for mattermost installation to become stable", wait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	err = s.waitAndInitializeInstallation(ctx, pr, request, installation, logger)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}

	// Get ClusterInstallation ID
	clusterInstallations, err := cloudClient.GetClusterInstallations(&cloudModel.GetClusterInstallationsRequest{
		InstallationID: installation.ID,
		Paging:         cloudModel.Paging{Page: 0, PerPage: 100},
	})
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get cluster installations")).ShouldReportError()
	}
	if len(clusterInstallations) == 0 {
		return request.WithError(errors.New("no cluster installations found")).ShouldReportError()
	}
	clusterInstallationID := clusterInstallations[0].ID

	// Wait for and install the plugin artifact
	logger.Info("Waiting for plugin artifact and installing")
	// Create a new context for plugin artifact wait (45 minutes)
	pluginCtx, pluginCancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer pluginCancel()
	pluginURL, err := s.waitForAndInstallPlugin(pluginCtx, pr, clusterInstallationID, logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "failed to install plugin")).ShouldReportError()
	}

	shortSHA := pr.Sha[0:7]

	// Post success comment with plugin info
	pluginTable := fmt.Sprintf("| Plugin | Version | Artifact |\n|---|---|---|\n| %s | %s | [Download](%s) |",
		pluginID, shortSHA, pluginURL)
	s.sendSpinwickSuccessComment(pr, installation, pluginTable)

	return request
}

// waitForAndInstallPlugin waits for the plugin artifact to be available on S3 and installs it
func (s *Server) waitForAndInstallPlugin(ctx context.Context, pr *model.PullRequest, clusterInstallationID string, logger logrus.FieldLogger) (string, error) {
	// Build the S3 URL for the plugin artifact
	shortSHA := pr.Sha[0:7]
	filename := fmt.Sprintf("%s-%s.tar.gz", pr.RepoName, shortSHA)
	s3URL := fmt.Sprintf("https://%s.s3.amazonaws.com/%s/%s", pluginS3Bucket, pr.RepoName, filename)

	logger.WithFields(logrus.Fields{
		"url": s3URL,
		"sha": shortSHA,
	}).Info("Waiting for plugin artifact on S3")

	// Wait for the artifact to be available on S3
	err := s.waitForS3Artifact(ctx, s3URL, logger)
	if err != nil {
		return "", errors.Wrap(err, "failed to wait for S3 artifact")
	}

	// Install the plugin using mmctl
	cloudClient := s.CloudClient
	pluginID := strings.TrimPrefix(pr.RepoName, pluginRepoPrefix)

	// Install the plugin using the S3 URL
	subcommand := []string{"plugin", "install-url", "-f", s3URL}
	output, err := cloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to install plugin")
		return "", errors.Wrap(err, "failed to install plugin via mmctl")
	}
	logger.WithField("output", string(output)).Info("Plugin installed successfully")

	// Enable the plugin
	subcommand = []string{"plugin", "enable", pluginID}
	output, err = cloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to enable plugin")
		return "", errors.Wrap(err, "failed to enable plugin via mmctl")
	}
	logger.WithField("output", string(output)).Info("Plugin enabled successfully")

	return s3URL, nil
}

// waitForS3Artifact waits for the artifact to be available on S3
func (s *Server) waitForS3Artifact(ctx context.Context, url string, logger logrus.FieldLogger) error {
	for {
		// Check if the artifact is available
		req, err := http.NewRequest("HEAD", url, nil)
		if err != nil {
			return errors.Wrap(err, "failed to create HEAD request")
		}

		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			logger.Info("Plugin artifact found on S3")
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}

		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for S3 artifact")
		case <-time.After(30 * time.Second):
			logger.Debug("S3 artifact not found yet. Waiting...")
		}
	}
}

// updatePluginSpinWick updates a SpinWick for a plugin repository
func (s *Server) updatePluginSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	// Use full repo name for ownerID to find existing installation
	ownerID := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)

	// Get existing installation
	installation, err := s.checkExistingInstallation(ownerID, logger)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}
	if installation == nil {
		return request.WithError(fmt.Errorf("no installation found with owner %s", ownerID)).ShouldReportError()
	}
	request.InstallationID = installation.ID

	logger = logger.WithField("installation_id", request.InstallationID)
	logger.Info("Updating plugin SpinWick")

	// Remove old messages
	serverNewCommitMessages := []string{
		"New commit detected.",
	}
	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	if errComments != nil {
		logger.WithError(err).Error("pr_error")
	} else {
		s.removeCommentsWithSpecificMessages(comments, serverNewCommitMessages, pr, logger)
	}
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "New commit detected. SpinWick will update the plugin if a new artifact is available.")

	// Get ClusterInstallation ID
	cloudClient := s.CloudClient
	clusterInstallations, err := cloudClient.GetClusterInstallations(&cloudModel.GetClusterInstallationsRequest{
		InstallationID: installation.ID,
		Paging:         cloudModel.Paging{Page: 0, PerPage: 100},
	})
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get cluster installations")).ShouldReportError()
	}
	if len(clusterInstallations) == 0 {
		return request.WithError(errors.New("no cluster installations found")).ShouldReportError()
	}
	clusterInstallationID := clusterInstallations[0].ID

	// Wait for and reinstall the plugin artifact
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pluginURL, err := s.waitForAndInstallPlugin(ctx, pr, clusterInstallationID, logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "failed to update plugin")).ShouldReportError()
	}

	// Remove old update messages
	if errComments == nil {
		serverUpdateMessage := []string{
			"Plugin test server updated",
		}
		s.removeCommentsWithSpecificMessages(comments, serverUpdateMessage, pr, logger)
	}

	// Extract plugin info from repo name and commit
	pluginID := strings.TrimPrefix(pr.RepoName, pluginRepoPrefix)
	shortSHA := pr.Sha[0:7]

	// Post update comment with updated plugin info
	pluginTable := fmt.Sprintf("Updated with git commit `%s`\n\n| Plugin | Version | Artifact |\n|---|---|---|\n| %s | %s | [Download](%s) |",
		pr.Sha, pluginID, shortSHA, pluginURL)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, fmt.Sprintf("Plugin test server updated!\n\n%s", pluginTable))

	return request
}

// destroyPluginSpinWick destroys a SpinWick for a plugin repository
func (s *Server) destroyPluginSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	// This can use the same logic as destroySpinWick since the installation is the same
	return s.destroySpinWick(pr, logger)
}

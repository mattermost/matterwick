// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/internal/cloudtools"
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

	sysadminPassword, userPassword, err := s.waitAndInitializeInstallation(ctx, pr, request, installation, logger)
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
	pluginResult := s.waitForAndInstallPlugin(pluginCtx, pr, clusterInstallationID, logger)

	shortSHA := pr.Sha[0:7]

	// Build plugin table and warning message
	pluginTable := fmt.Sprintf("| Plugin | Version | Artifact |\n|---|---|---|\n| %s | %s | [Download](%s) |",
		pluginID, shortSHA, pluginResult.PluginURL)

	var extraInfo string
	if !pluginResult.Success {
		// Installation created but plugin installation/enablement failed
		warningMsg := "\n\n:warning: **Plugin Installation Issue**\n\n"
		warningMsg += "The test server was created successfully, but there was an issue installing or enabling the plugin automatically:\n\n"

		if pluginResult.InstallError != nil {
			warningMsg += fmt.Sprintf("- **Install Error:** %s\n", pluginResult.InstallError.Error())
		}
		if pluginResult.EnableError != nil {
			warningMsg += fmt.Sprintf("- **Enable Error:** %s\n", pluginResult.EnableError.Error())
		}

		warningMsg += fmt.Sprintf("\n**You can manually install the plugin:**\n1. Download the plugin artifact from the link above\n2. Upload it to your test server at %s\n3. Enable it in System Console > Plugins\n\n", installation.ID)
		warningMsg += "Future commits will still attempt to automatically update the plugin."

		extraInfo = pluginTable + warningMsg
		logger.WithFields(logrus.Fields{
			"install_error": pluginResult.InstallError,
			"enable_error":  pluginResult.EnableError,
		}).Warn("Plugin installation/enablement failed, but server is available for manual installation")
	} else {
		extraInfo = pluginTable
		// Check for config errors even when plugin install/enable succeeded
		if pluginResult.ConfigError != nil {
			warningMsg := "\n\n:warning: **Plugin Configuration Notice**\n\n"
			warningMsg += "The plugin was installed and enabled, but automatic configuration failed:\n\n"
			warningMsg += fmt.Sprintf("- **Config Error:** %s\n", pluginResult.ConfigError.Error())
			warningMsg += "\nYou may need to manually configure the plugin in System Console."
			extraInfo += warningMsg
			logger.WithError(pluginResult.ConfigError).Warn("Plugin configuration failed")
		}
	}

	s.sendSpinwickSuccessToMattermost(pr, installation, sysadminPassword, userPassword, extraInfo, logger)

	return request
}

// waitForAndInstallPlugin waits for the plugin artifact to be available on S3 and installs it
func (s *Server) waitForAndInstallPlugin(ctx context.Context, pr *model.PullRequest, clusterInstallationID string, logger logrus.FieldLogger) *model.PluginInstallResult {
	// Build the S3 URL for the plugin artifact
	shortSHA := pr.Sha[0:7]
	filename := fmt.Sprintf("%s-%s.tar.gz", pr.RepoName, shortSHA)
	s3URL := fmt.Sprintf("https://%s.s3.amazonaws.com/%s/%s", pluginS3Bucket, pr.RepoName, filename)

	logger.WithFields(logrus.Fields{
		"url": s3URL,
		"sha": shortSHA,
	}).Info("Waiting for plugin artifact on S3")

	result := &model.PluginInstallResult{
		PluginURL:     s3URL,
		Success:       false,
		ArtifactFound: false,
	}

	// Wait for the artifact to be available on S3
	err := s.waitForS3Artifact(ctx, s3URL, logger)
	if err != nil {
		result.InstallError = errors.Wrap(err, "failed to wait for S3 artifact")
		return result
	}
	result.ArtifactFound = true

	// Install the plugin using mmctl
	cloudClient := s.CloudClient

	// Determine the plugin ID - check config mapping first, then fall back to trimming prefix
	pluginID, exists := s.Config.PluginRepoToIDMapping[pr.RepoName]
	if !exists {
		pluginID = strings.TrimPrefix(pr.RepoName, pluginRepoPrefix)
		logger.WithField("pluginID", pluginID).Debug("Using calculated plugin ID (no mapping found)")
	} else {
		logger.WithField("pluginID", pluginID).Debug("Using mapped plugin ID from config")
	}

	// Install the plugin using the S3 URL
	subcommand := []string{"--local", "plugin", "install-url", "-f", s3URL}
	output, err := cloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to install plugin")
		result.InstallError = errors.Wrap(err, "failed to install plugin via mmctl")
		return result
	}
	logger.WithField("output", string(output)).Info("Plugin installed successfully")

	// Wait for 20 seconds to allow the system to stabilize after plugin installation
	time.Sleep(20 * time.Second)

	// Enable the plugin
	subcommand = []string{"--local", "plugin", "enable", pluginID}
	output, err = cloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to enable plugin")
		result.EnableError = errors.Wrap(err, "failed to enable plugin via mmctl")
		return result
	}
	logger.WithField("output", string(output)).Info("Plugin enabled successfully")

	// Apply plugin-specific configuration if defined
	if configErr := s.applyPluginConfig(clusterInstallationID, pr.RepoName, logger); configErr != nil {
		logger.WithError(configErr).Warn("Failed to apply plugin configuration")
		result.ConfigError = configErr
		// Don't fail the overall operation - plugin is already installed and enabled
	}

	result.Success = true
	return result
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

// applyPluginConfig applies plugin-specific configuration after plugin installation
func (s *Server) applyPluginConfig(clusterInstallationID string, repoName string, logger logrus.FieldLogger) error {
	pluginConfig, exists := s.Config.PluginRepoConfigs[repoName]
	if !exists {
		logger.Debug("No plugin config found for repository")
		return nil
	}

	if pluginConfig.PluginID == "" || len(pluginConfig.Settings) == 0 {
		logger.Debug("Plugin config empty, skipping")
		return nil
	}

	logger.WithFields(logrus.Fields{
		"repo":     repoName,
		"pluginID": pluginConfig.PluginID,
	}).Info("Applying plugin configuration")

	// Build the full config structure: PluginSettings.Plugins.<pluginID>
	fullConfig := map[string]interface{}{
		"PluginSettings": map[string]interface{}{
			"Plugins": map[string]interface{}{
				pluginConfig.PluginID: pluginConfig.Settings,
			},
		},
	}

	configJSON, err := json.Marshal(fullConfig)
	if err != nil {
		return errors.Wrap(err, "failed to marshal plugin config")
	}

	// Use mmctl config patch with JSON input
	subcommand := []string{"--local", "config", "patch", string(configJSON)}
	output, err := s.CloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to apply plugin config")
		return errors.Wrap(err, "failed to apply plugin configuration via mmctl")
	}

	logger.WithField("output", string(output)).Info("Plugin configuration applied successfully")
	return nil
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

	pluginResult := s.waitForAndInstallPlugin(ctx, pr, clusterInstallationID, logger)

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

	// Build update message with plugin info
	pluginTable := fmt.Sprintf("Updated with git commit `%s`\n\n| Plugin | Version | Artifact |\n|---|---|---|\n| %s | %s | [Download](%s) |",
		pr.Sha, pluginID, shortSHA, pluginResult.PluginURL)

	var updateMessage string
	if !pluginResult.Success {
		// Plugin update failed, but installation is still available
		updateMessage = "Plugin test server update attempted, but encountered an issue:\n\n"

		if pluginResult.InstallError != nil {
			updateMessage += fmt.Sprintf(":warning: **Install Error:** %s\n\n", pluginResult.InstallError.Error())
		}
		if pluginResult.EnableError != nil {
			updateMessage += fmt.Sprintf(":warning: **Enable Error:** %s\n\n", pluginResult.EnableError.Error())
		}

		updateMessage += "The test server is still available. You can manually download and install the updated plugin using the artifact link below.\n\n"
		updateMessage += pluginTable

		logger.WithFields(logrus.Fields{
			"install_error": pluginResult.InstallError,
			"enable_error":  pluginResult.EnableError,
		}).Warn("Plugin update failed, but server remains available")
	} else {
		updateMessage = fmt.Sprintf("Plugin test server updated!\n\n%s", pluginTable)
		// Check for config errors even when plugin update succeeded
		if pluginResult.ConfigError != nil {
			updateMessage += "\n\n:warning: **Plugin Configuration Notice**\n\n"
			updateMessage += "The plugin was updated, but automatic configuration failed:\n\n"
			updateMessage += fmt.Sprintf("- **Config Error:** %s\n", pluginResult.ConfigError.Error())
			updateMessage += "\nYou may need to manually configure the plugin in System Console."
			logger.WithError(pluginResult.ConfigError).Warn("Plugin configuration failed during update")
		}
	}

	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, updateMessage)

	return request
}

// destroyPluginSpinWick destroys a SpinWick for a plugin repository
func (s *Server) destroyPluginSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	// This can use the same logic as destroySpinWick since the installation is the same
	return s.destroySpinWick(pr, logger)
}

// uploadPluginToExternalInstallation uploads a plugin artifact to an external installation by DNS
// This does NOT apply config blobs - external installations manage their own configuration
func (s *Server) uploadPluginToExternalInstallation(ctx context.Context, pr *model.PullRequest, dnsHostname string, logger logrus.FieldLogger) *model.PluginInstallResult {
	result := &model.PluginInstallResult{
		Success:       false,
		ArtifactFound: false,
	}

	// Build the S3 URL for the plugin artifact
	shortSHA := pr.Sha[0:7]
	filename := fmt.Sprintf("%s-%s.tar.gz", pr.RepoName, shortSHA)
	s3URL := fmt.Sprintf("https://%s.s3.amazonaws.com/%s/%s", pluginS3Bucket, pr.RepoName, filename)
	result.PluginURL = s3URL

	logger.WithFields(logrus.Fields{
		"url":          s3URL,
		"sha":          shortSHA,
		"dns_hostname": dnsHostname,
	}).Info("Uploading plugin to external installation")

	// Look up installation by DNS
	installation, err := cloudtools.GetInstallationByDNS(s.CloudClient, dnsHostname)
	if err != nil {
		result.InstallError = errors.Wrap(err, "failed to look up installation by DNS")
		return result
	}
	if installation == nil {
		result.InstallError = fmt.Errorf("no installation found with DNS hostname: %s", dnsHostname)
		return result
	}

	logger = logger.WithField("installation_id", installation.ID)

	// Check installation is stable
	if installation.State != cloudModel.InstallationStateStable {
		result.InstallError = fmt.Errorf("installation is not stable (current state: %s)", installation.State)
		return result
	}

	// Get ClusterInstallation ID
	clusterInstallations, err := s.CloudClient.GetClusterInstallations(&cloudModel.GetClusterInstallationsRequest{
		InstallationID: installation.ID,
		Paging:         cloudModel.Paging{Page: 0, PerPage: 100},
	})
	if err != nil {
		result.InstallError = errors.Wrap(err, "unable to get cluster installations")
		return result
	}
	if len(clusterInstallations) == 0 {
		result.InstallError = errors.New("no cluster installations found")
		return result
	}
	clusterInstallationID := clusterInstallations[0].ID

	// Wait for S3 artifact to be available (reuse existing function)
	err = s.waitForS3Artifact(ctx, s3URL, logger)
	if err != nil {
		result.InstallError = errors.Wrap(err, "failed to wait for S3 artifact")
		return result
	}
	result.ArtifactFound = true

	// Determine the plugin ID - check config mapping first, then fall back to trimming prefix
	pluginID, exists := s.Config.PluginRepoToIDMapping[pr.RepoName]
	if !exists {
		pluginID = strings.TrimPrefix(pr.RepoName, pluginRepoPrefix)
	}

	// Install the plugin using the S3 URL
	subcommand := []string{"--local", "plugin", "install-url", "-f", s3URL}
	output, err := s.CloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to install plugin")
		result.InstallError = errors.Wrap(err, "failed to install plugin via mmctl")
		return result
	}
	logger.WithField("output", string(output)).Info("Plugin installed successfully")

	// Wait for system to stabilize
	time.Sleep(20 * time.Second)

	// Enable the plugin
	subcommand = []string{"--local", "plugin", "enable", pluginID}
	output, err = s.CloudClient.ExecClusterInstallationCLI(clusterInstallationID, "mmctl", subcommand)
	if err != nil {
		logger.WithError(err).WithField("output", string(output)).Error("Failed to enable plugin")
		result.EnableError = errors.Wrap(err, "failed to enable plugin via mmctl")
		return result
	}
	logger.WithField("output", string(output)).Info("Plugin enabled successfully")

	// NOTE: We deliberately do NOT apply config blobs for external installations
	// External installations manage their own configuration

	result.Success = true
	return result
}

// handleUploadPluginToExternal handles the /spinwick upload --dns <hostname> command
func (s *Server) handleUploadPluginToExternal(pr *model.PullRequest, dnsHostname string) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo_name":    pr.RepoName,
		"pr":           pr.Number,
		"dns_hostname": dnsHostname,
	})

	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number,
		fmt.Sprintf("Uploading plugin to external installation at `%s`...", dnsHostname))

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	result := s.uploadPluginToExternalInstallation(ctx, pr, dnsHostname, logger)

	shortSHA := pr.Sha[0:7]
	pluginID := strings.TrimPrefix(pr.RepoName, pluginRepoPrefix)

	var msg string
	if result.Success {
		msg = fmt.Sprintf("Plugin uploaded successfully to `%s`!\n\n", dnsHostname)
		msg += fmt.Sprintf("| Plugin | Version | Artifact |\n|---|---|---|\n| %s | %s | [Download](%s) |",
			pluginID, shortSHA, result.PluginURL)
		msg += "\n\n**Note:** Configuration must be managed manually on external installations."
	} else {
		msg = fmt.Sprintf("Failed to upload plugin to `%s`\n\n", dnsHostname)
		if result.InstallError != nil {
			msg += fmt.Sprintf("**Error:** %s\n", result.InstallError.Error())
		}
		if result.EnableError != nil {
			msg += fmt.Sprintf("**Enable Error:** %s\n", result.EnableError.Error())
		}
		if result.PluginURL != "" {
			msg += fmt.Sprintf("\n**Artifact:** [Download](%s)", result.PluginURL)
		}
	}

	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/mattermost-server/v5/mlog"
	mattermostModel "github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/matterwick/internal/cloudtools"
	"github.com/mattermost/matterwick/internal/spinwick"
	"github.com/mattermost/matterwick/model"

	"github.com/google/go-github/v28/github"
	"github.com/pkg/errors"

	// K8s packages for CWS
	"github.com/mattermost/mattermost-cloud/k8s"
	log "github.com/sirupsen/logrus"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *Server) handleCreateSpinWick(pr *model.PullRequest, size string, withLicense bool) {

	if pr.State == "closed" {
		mlog.Info("PR is closed/merged, will not create a test server", mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number))
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "PR is closed/merged not creating a SpinWick Test server")
		return
	}

	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}
	if pr.RepoName == "customer-web-server" {
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Creating a SpinWick test CWS")
		request = s.createKubeSpinWick(pr)
	} else {
		if withLicense {
			s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Creating a new HA SpinWick test server using Mattermost Cloud.")
		} else {
			s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Creating a new SpinWick test server using Mattermost Cloud.")
		}

		request = s.createSpinWick(pr, size, withLicense)
	}

	if request.Error != nil {
		if request.Aborted {
			mlog.Warn("Aborted creation of SpinWick", mlog.String("abort_message", request.Error.Error()), mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number), mlog.String("installation_id", request.InstallationID))
		} else {
			mlog.Error("Failed to create SpinWick", mlog.Err(request.Error), mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number), mlog.String("installation_id", request.InstallationID))
		}
		comments, err := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
		if err != nil {
			mlog.Error("Error getting comments", mlog.Err(err))
		} else {
			s.removeOldComments(comments, pr)
		}
		for _, label := range pr.Labels {
			if s.isSpinWickLabel(label) {
				s.removeLabel(pr.RepoOwner, pr.RepoName, pr.Number, label)
			}
		}
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, s.Config.SetupSpinmintFailedMessage)

		if request.ReportError {
			additionalFields := map[string]string{
				"Installation ID": request.InstallationID,
			}
			s.logPrettyErrorToMattermost("[ SpinWick ] Creation Failed", pr, request.Error, additionalFields)
		}
	}

}

func (s *Server) createKubeSpinWick(pr *model.PullRequest) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	logger := log.WithField("PR", pr.RepoName+": #"+string(pr.Number))
	kc, err := s.newClient(logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error occurred while getting Kube Client"))
	}

	namespaceName := s.makeSpinWickID(pr.RepoName, pr.Number)
	namespace, err := getOrCreateNamespace(kc, namespaceName)

	if err != nil {
		request.Error = err
		return request.WithError(errors.Wrap(err, "Error occurred whilst creating namespace")).ShouldReportError()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	version := ""
	image := "mattermost/cws-test"

	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}

	prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image)
	if errImage != nil {
		return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	version = s.Builds.getInstallationVersion(prNew)

	deployment := Deployment{
		namespace.GetName(),
		version,
		"/tmp/cws_deployment" + namespace.GetName() + ".yaml",
		s.Config.CWS,
	}

	template, err := template.ParseFiles("/matterwick/templates/cws/cws_deployment.tmpl")
	if err != nil {
		mlog.Error("Error loading deployment template ", mlog.Err(err))
	}

	file, err := os.Create(deployment.DeployFilePath)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error creating deployment file")).ShouldReportError()
	}

	err = template.Execute(file, deployment)
	if err != nil {
		mlog.Error("Error executing template ", mlog.Err(err))
	}
	file.Close()

	request.InstallationID = namespace.GetName()

	deployFile := k8s.ManifestFile{
		Path:            deployment.DeployFilePath,
		DeployNamespace: deployment.Namespace,
	}
	err = kc.CreateFromFile(deployFile, "")
	defer os.Remove(deployment.DeployFilePath)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error deploying from manifest template")).ShouldReportError()
	}

	mlog.Info("Deployment created successfully. Cleanup complete")

	lbURL, _ := waitForIPAssignment(kc, deployment)

	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	commentsToDelete := []string{"Creating a SpinWick test CWS"}
	if errComments != nil {
		mlog.Error("pr_error", mlog.Err(err))
	} else {
		s.removeCommentsWithSpecificMessages(comments, commentsToDelete, pr)
	}

	spinwickURL := fmt.Sprintf("http://%s", lbURL)
	msg := fmt.Sprintf("CWS test server created! :tada:\n\nAccess here: %s\n\n", spinwickURL)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	request.InstallationID = deployment.Namespace
	return request
}

// createSpinwick creates a SpinWick with the following behavior:
// - no cloud installation found = installation is created
// - cloud installation found = actual ID string and no error
// - any errors = error is returned
func (s *Server) createSpinWick(pr *model.PullRequest, size string, withLicense bool) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}
	ownerID := s.makeSpinWickID(pr.RepoName, pr.Number)
	id, _, err := cloudtools.GetInstallationIDFromOwnerID(s.Config.ProvisionerServer, s.Config.AWSAPIKey, ownerID)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}
	if id != "" {
		return request.WithInstallationID(id).WithError(fmt.Errorf("Already found a installation belonging to %s", ownerID)).IntentionalAbort()
	}
	request.InstallationID = id

	// Remove old message to reduce the amount of similar messages and avoid confusion
	serverNewCommitMessages := []string{
		"Test server destroyed",
	}
	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	if errComments != nil {
		mlog.Error("pr_error", mlog.Err(err))
	} else {
		s.removeCommentsWithSpecificMessages(comments, serverNewCommitMessages, pr)
	}

	mlog.Info("No SpinWick found for this PR. Creating a new one.")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	// set the version to master
	version := "master"
	image := "mattermost/mattermost-enterprise-edition"

	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}
	// if is server or webapp then set version to the PR git commit hash
	if pr.RepoName == "mattermost-webapp" {
		mlog.Info("Waiting for docker image to set up SpinWick", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))

		// Waiting for Enterprise Image
		prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image)
		if errImage != nil {
			return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
		}

		version = s.Builds.getInstallationVersion(prNew)
	} else if pr.RepoName == "mattermost-server" {
		mlog.Info("Waiting for docker image to set up SpinWick", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))

		ctxEnterprise, cancelEnterprise := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancelEnterprise()
		// Waiting for Enterprise Image
		prNew, errImage := s.Builds.waitForImage(ctxEnterprise, s, reg, pr, image)
		if errImage != nil {
			if withLicense {
				s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Enterprise Edition Image not available in the 30 minutes timeframe.\nPlease check if the EE Pipeline was triggered and if not please trigger and re-add the `Setup HA Cloud Test Server` again.")
				return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting. Check if EE pipeline ran")).IntentionalAbort()
			}

			mlog.Warn("Did not find the EE image, fallback to TE", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName), mlog.String("sha", pr.Sha))
			s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Enterprise Edition Image not available in the 30 minutes timeframe, checking the Team Edition Image and if available will use that.")
			//fallback to TE
			image = "mattermost/mattermost-team-edition"
			ctxTeam, cancelTeam := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancelTeam()
			prNew, errImage = s.Builds.waitForImage(ctxTeam, s, reg, pr, image)
			if errImage != nil {
				mlog.Warn("Did not find TE image", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName), mlog.String("sha", pr.Sha))
				return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
			}
		}

		version = s.Builds.getInstallationVersion(prNew)
	}

	mlog.Info("Provisioning Server - Installation request")

	headers := map[string]string{
		"x-api-key": s.Config.AWSAPIKey,
	}
	cloudClient := cloudModel.NewClientWithHeaders(s.Config.ProvisionerServer, headers)

	// TODO: (cpanato) add the group permission in the AUTH
	// var groupID string
	// var group *cloudModel.Group
	// if len(s.Config.CloudGroupID) != 0 {
	// 	group, err = cloudClient.GetGroup(s.Config.CloudGroupID)
	// 	if err != nil {
	// 		return request.WithError(errors.Wrapf(err, "unable to get group with ID %s", s.Config.CloudGroupID))
	// 	}
	// 	if group == nil {
	// 		return request.WithError(fmt.Errorf("group with ID %s does not exist", s.Config.CloudGroupID))
	// 	}
	// 	groupID = s.Config.CloudGroupID
	// }

	installationRequest := &cloudModel.CreateInstallationRequest{
		OwnerID:  ownerID,
		Version:  version,
		Image:    image,
		DNS:      fmt.Sprintf("%s.%s", ownerID, s.Config.DNSNameTestServer),
		Size:     size,
		Affinity: "multitenant",
	}
	if withLicense {
		installationRequest.License = s.Config.SpinWickHALicense
	}

	// TODO: (cpanato) Remove this when the above code comment is fixed
	if len(s.Config.CloudGroupID) != 0 {
		installationRequest.GroupID = s.Config.CloudGroupID
	}

	installation, err := cloudClient.CreateInstallation(installationRequest)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to make the installation creation request to the provisioning server")).ShouldReportError()
	}
	request.InstallationID = installation.ID
	mlog.Info("Provisioner Server - installation request", mlog.String("InstallationID", request.InstallationID))

	wait := 1200
	mlog.Info("Waiting for mattermost installation to become stable", mlog.Int("wait_seconds", wait))
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	s.waitForInstallationStable(ctx, pr, request)
	if request.Error != nil {
		return request.WithError(errors.Wrap(request.Error, "error waiting for installation to become stable"))
	}

	spinwickURL := fmt.Sprintf("https://%s.%s", s.makeSpinWickID(pr.RepoName, pr.Number), s.Config.DNSNameTestServer)
	err = s.initializeMattermostTestServer(spinwickURL, pr.Number)
	if err != nil {
		return request.WithError(errors.Wrap(err, "failed to initialize the Installation")).ShouldReportError()
	}
	userTable := "| Account Type | Username | Password |\n|---|---|---|\n| Admin | sysadmin | Sys@dmin123 |\n| User | user-1 | User-1@123 |"
	msg := fmt.Sprintf("Mattermost test server created! :tada:\n\nAccess here: %s\n\n%s", spinwickURL, userTable)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	return request
}

func (s *Server) handleUpdateSpinWick(pr *model.PullRequest, withLicense bool) {
	// other repos we are not updating
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	if pr.RepoName == "customer-web-server" {
		request = s.updateKubeSpinWick(pr)
	} else {
		request = s.updateSpinWick(pr, withLicense)
	}

	if request.Error != nil {
		if request.Aborted {
			mlog.Warn("Aborted update of SpinWick", mlog.String("abort_message", request.Error.Error()), mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number), mlog.String("installation_id", request.InstallationID))
		} else {
			mlog.Error("Failed to update SpinWick", mlog.Err(request.Error), mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number), mlog.String("installation_id", request.InstallationID))
		}
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, s.Config.SetupSpinmintFailedMessage)
		if request.ReportError {
			additionalFields := map[string]string{
				"Installation ID": request.InstallationID,
			}
			s.logPrettyErrorToMattermost("[ SpinWick ] Update Failed", pr, request.Error, additionalFields)
		}
	}
}

func (s *Server) updateKubeSpinWick(pr *model.PullRequest) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}
	logger := log.WithField("PR", pr.RepoName+": #"+string(pr.Number))

	kc, err := s.newClient(logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error occurred while getting Kube Client"))
	}
	namespaceName := s.makeSpinWickID(pr.RepoName, pr.Number)
	namespaceExists, err := namespaceExists(kc, namespaceName)

	if err != nil {
		request.Error = err
		return request
	}

	if !namespaceExists {
		return request.WithError(fmt.Errorf("No namespace found with name %s", namespaceName)).ShouldReportError()
	}

	request.InstallationID = namespaceName

	// Now that we know this namespace exists, notify via comment that we are attempting to upgrade the deployment
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "New commit detected. SpinWick will upgrade if the updated docker image is available.")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	version := ""
	image := "mattermost/cws-test"

	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}

	prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image)
	if errImage != nil {
		return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	version = s.Builds.getInstallationVersion(prNew)

	deployClient := kc.Clientset.AppsV1().Deployments(namespaceName)
	deployment, err := deployClient.Get("cws-test", metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		mlog.Info("Attempted to update a deployment that does not exist")
		return request.WithError(errors.Wrap(err, "Attempted to update a deployment that does not exist")).ShouldReportError()
	}

	for idx := range deployment.Spec.Template.Spec.Containers {
		deployment.Spec.Template.Spec.Containers[idx].Image = image + ":" + version
	}

	for idx := range deployment.Spec.Template.Spec.InitContainers {
		deployment.Spec.Template.Spec.InitContainers[idx].Image = image + ":" + version
	}

	_, err = deployClient.Update(deployment)

	if err != nil {
		return request.WithError(errors.Wrap(err, "failed while updating deployment with latest image")).ShouldReportError()
	}

	return request
}

// updateSpinWick updates a SpinWick with the following behavior:
// - no cloud installation found = error is returned
// - cloud installation found and updated = actual ID string and no error
// - any errors = error is returned
func (s *Server) updateSpinWick(pr *model.PullRequest, withLicense bool) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	ownerID := s.makeSpinWickID(pr.RepoName, pr.Number)
	id, image, err := cloudtools.GetInstallationIDFromOwnerID(s.Config.ProvisionerServer, s.Config.AWSAPIKey, ownerID)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}
	if id == "" {
		return request.WithError(fmt.Errorf("no installation found with owner %s", ownerID)).ShouldReportError()
	}
	request.InstallationID = id

	mlog.Info("Sleeping a bit to wait for the build process to start", mlog.Int("pr", pr.Number), mlog.String("sha", pr.Sha))
	time.Sleep(60 * time.Second)

	// Remove old message to reduce the amount of similar messages and avoid confusion
	serverNewCommitMessages := []string{
		"New commit detected.",
	}
	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	if errComments != nil {
		mlog.Error("pr_error", mlog.Err(err))
	} else {
		s.removeCommentsWithSpecificMessages(comments, serverNewCommitMessages, pr)
	}
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "New commit detected. SpinWick will upgrade if the updated docker image is available.")

	reg, err := s.Builds.dockerRegistryClient(s)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get docker registry client")).ShouldReportError()
	}

	mlog.Info("Waiting for docker image to update SpinWick", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName))

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pr, err = s.Builds.waitForImage(ctx, s, reg, pr, image)
	if err != nil {
		return request.WithError(errors.Wrap(err, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	installationVersion := s.Builds.getInstallationVersion(pr)

	upgradeRequest := &cloudModel.PatchInstallationRequest{
		Version: &installationVersion,
		Image:   &image,
	}
	if withLicense {
		upgradeRequest.License = &s.Config.SpinWickHALicense
	}

	// Final upgrade check
	// Let's get the installation state one last time. If the version matches
	// what we want then another process already updated it.
	headers := map[string]string{
		"x-api-key": s.Config.AWSAPIKey,
	}
	cloudClient := cloudModel.NewClientWithHeaders(s.Config.ProvisionerServer, headers)
	installation, err := cloudClient.GetInstallation(request.InstallationID, &cloudModel.GetInstallationRequest{})
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get installation")).ShouldReportError()
	}
	if installation.Version == *upgradeRequest.Version {
		return request.WithError(errors.New("another process already updated the installation version. Aborting")).IntentionalAbort()
	}

	mlog.Info("Provisioning Server - Upgrade request", mlog.String("SHA", pr.Sha))

	_, err = cloudClient.UpdateInstallation(request.InstallationID, upgradeRequest)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to make upgrade request to provisioning server")).ShouldReportError()
	}

	wait := 600
	mlog.Info("Waiting for mattermost installation to become stable", mlog.Int("wait_seconds", wait))
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	s.waitForInstallationStable(ctx, pr, request)
	if request.Error != nil {
		return request.WithError(errors.Wrap(request.Error, "error waiting for installation to become stable"))
	}

	// Remove old message to reduce the amount of similar messages and avoid confusion
	if errComments == nil {
		serverUpdateMessage := []string{
			"Mattermost test server updated",
		}
		s.removeCommentsWithSpecificMessages(comments, serverUpdateMessage, pr)
	}

	mmURL := fmt.Sprintf("https://%s.%s", s.makeSpinWickID(pr.RepoName, pr.Number), s.Config.DNSNameTestServer)
	msg := fmt.Sprintf("Mattermost test server updated with git commit `%s`.\n\nAccess here: %s", pr.Sha, mmURL)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	return request
}

func (s *Server) handleDestroySpinWick(pr *model.PullRequest) {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	if pr.RepoName == "customer-web-server" {
		request = s.destroyKubeSpinWick(pr)
	} else {
		request = s.destroySpinWick(pr)
	}

	if request.Error != nil {
		if request.Aborted {
			mlog.Warn("Aborted deletion of SpinWick", mlog.String("abort_message", request.Error.Error()), mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number), mlog.String("installation_id", request.InstallationID))
		} else {
			mlog.Error("Failed to delete SpinWick", mlog.Err(request.Error), mlog.String("repo_name", pr.RepoName), mlog.Int("pr", pr.Number), mlog.String("installation_id", request.InstallationID))
		}
		if request.ReportError {
			additionalFields := map[string]string{
				"Installation ID": request.InstallationID,
			}
			s.logPrettyErrorToMattermost("[ SpinWick ] Destroy Failed", pr, request.Error, additionalFields)
		}
	}
}

func (s *Server) destroyKubeSpinWick(pr *model.PullRequest) *spinwick.Request {
	mlog.Info("Received request to destroy kubernetes namespace")
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	logger := log.WithField("PR", pr.RepoName+": #"+string(pr.Number))

	namespaceName := s.makeSpinWickID(pr.RepoName, pr.Number)

	kc, err := s.newClient(logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error occurred while getting Kube Client"))
	}
	namespaceExists, err := namespaceExists(kc, namespaceName)

	if err != nil {
		return request.WithError(errors.Wrap(err, "Failed while getting namespace"))
	}

	if !namespaceExists {
		request.InstallationID = ""
		return request
	}

	err = deleteNamespace(kc, namespaceName)
	if err != nil {
		mlog.Error("Failed while deleting namespace", mlog.Err(err))
		request.Error = err
		return request
	}
	request.InstallationID = namespaceName
	mlog.Info("Kube namespace " + namespaceName + " has been destroyed")

	// Old comments created by MatterWick user will be deleted here.
	s.commentLock.Lock()
	defer s.commentLock.Unlock()
	comments, _, err := newGithubClient(s.Config.GithubAccessToken).Issues.ListComments(context.Background(), pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get list of old comments")).ShouldReportError()
	}
	s.removeOldComments(comments, pr)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Spinwick Kubernetes namespace "+namespaceName+" has been destroyed")
	return request
}

// destroySpinwick destroys a SpinWick with the following behavior:
// - no cloud installation found = empty ID string and no error
// - cloud installation found and deleted = actual ID string and no error
// - any errors = error is returned
func (s *Server) destroySpinWick(pr *model.PullRequest) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	ownerID := s.makeSpinWickID(pr.RepoName, pr.Number)
	id, _, err := cloudtools.GetInstallationIDFromOwnerID(s.Config.ProvisionerServer, s.Config.AWSAPIKey, ownerID)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}
	if id == "" {
		return request.WithInstallationID(id).WithError(errors.New("No SpinWick found for this PR. Skipping deletion")).IntentionalAbort()
	}
	request.InstallationID = id

	mlog.Info("Destroying SpinWick", mlog.Int("pr", pr.Number), mlog.String("repo_owner", pr.RepoOwner), mlog.String("repo_name", pr.RepoName), mlog.String("installation_id", request.InstallationID))

	headers := map[string]string{
		"x-api-key": s.Config.AWSAPIKey,
	}
	cloudClient := cloudModel.NewClientWithHeaders(s.Config.ProvisionerServer, headers)
	err = cloudClient.DeleteInstallation(request.InstallationID)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to make installation delete request to provisioning server")).ShouldReportError()
	}

	// Old comments created by MatterWick user will be deleted here.
	s.commentLock.Lock()
	defer s.commentLock.Unlock()

	comments, _, err := newGithubClient(s.Config.GithubAccessToken).Issues.ListComments(context.Background(), pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get list of old comments")).ShouldReportError()
	}
	s.removeOldComments(comments, pr)

	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, s.Config.DestroyedSpinmintMessage)

	return request
}

func (s *Server) waitForInstallationStable(ctx context.Context, pr *model.PullRequest, request *spinwick.Request) {
	channel, err := s.requestCloudWebhookChannel(request.InstallationID)
	if err != nil {
		request.WithError(err).ShouldReportError()
		return
	}
	defer s.removeCloudWebhookChannel(request.InstallationID)

	for {
		select {
		case <-ctx.Done():
			request.WithError(errors.New("timed out waiting for the mattermost installation to stabilize")).ShouldReportError()
			return
		case payload := <-channel:
			if payload.ID != request.InstallationID {
				continue
			}

			mlog.Info("Installation changed state", mlog.String("installation", request.InstallationID), mlog.String("state", payload.NewState))

			switch payload.NewState {
			case cloudModel.InstallationStateStable:
				return
			case cloudModel.InstallationStateCreationFailed:
				request.WithError(errors.New("the installation creation failed")).ShouldReportError()
				return
			case cloudModel.InstallationStateDeletionRequested,
				cloudModel.InstallationStateDeletionInProgress,
				cloudModel.InstallationStateDeleted:
				// Another process may have deleted the installation. Let's check.
				pr, err = s.GetUpdateChecks(pr.RepoOwner, pr.RepoName, pr.Number)
				if err != nil {
					request.WithError(errors.Wrapf(err, "received state update %s, but was unable to check PR labels", payload.NewState)).ShouldReportError()
					return
				}
				if !s.isSpinWickLabelInLabels(pr.Labels) {
					request.WithError(errors.New("the SpinWick label has been removed. Aborting")).IntentionalAbort()
					return
				}
			case cloudModel.InstallationStateCreationNoCompatibleClusters:
				s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "No Kubernetes clusters available at the moment, please contact the Mattermost Cloud Team or wait a bit.")
				request.WithError(errors.New("no k8s clusters available")).IntentionalAbort()
				return
			}
		}
	}
}

func makeRequest(method, url string, payload io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Server) initializeMattermostTestServer(mmURL string, prNumber int) error {
	mlog.Info("Initializing Mattermost installation")

	wait := 600
	mlog.Info("Waiting up to 600 seconds for DNS to propagate")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	mmHost, _ := url.Parse(mmURL)
	err := checkDNS(ctx, fmt.Sprintf("%s:443", mmHost.Host))
	if err != nil {
		return errors.Wrap(err, "timed out waiting for DNS to propagate for installation")
	}

	client := mattermostModel.NewAPIv4Client(mmURL)

	//check if Mattermost is available
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	err = checkMMPing(ctx, client)
	if err != nil {
		return errors.Wrap(err, "failed to get mattermost ping response")
	}

	user := &mattermostModel.User{
		Username: "sysadmin",
		Email:    "sysadmin@example.mattermost.com",
		Password: "Sys@dmin123",
	}
	_, response := client.CreateUser(user)
	if response.StatusCode != 201 {
		return fmt.Errorf("error creating the initial mattermost user: status code = %d, message = %s", response.StatusCode, response.Error.Message)
	}

	client.Logout()
	userLogged, response := client.Login("sysadmin", "Sys@dmin123")
	if response.StatusCode != 200 {
		return fmt.Errorf("error logging in with initial mattermost user: status code = %d, message = %s", response.StatusCode, response.Error.Message)
	}

	teamName := fmt.Sprintf("pr%d", prNumber)
	team := &mattermostModel.Team{
		Name:        teamName,
		DisplayName: teamName,
		Type:        "O",
	}
	firstTeam, response := client.CreateTeam(team)
	if response.StatusCode != 201 {
		return fmt.Errorf("error creating the initial team: status code = %d, message = %s", response.StatusCode, response.Error.Message)
	}

	_, response = client.AddTeamMember(firstTeam.Id, userLogged.Id)
	if response.StatusCode != 201 {
		return fmt.Errorf("error adding sysadmin to the initial team: status code = %d, message = %s", response.StatusCode, response.Error.Message)
	}

	testUser := &mattermostModel.User{
		Username: "user-1",
		Email:    "user-1@example.mattermost.com",
		Password: "User-1@123",
	}
	testUser, response = client.CreateUser(testUser)
	if response.StatusCode != 201 {
		return fmt.Errorf("error creating the standard test user: status code = %d, message = %s", response.StatusCode, response.Error.Message)
	}
	_, response = client.AddTeamMember(firstTeam.Id, testUser.Id)
	if response.StatusCode != 201 {
		return fmt.Errorf("error adding standard test user to the initial team: status code = %d, message = %s", response.StatusCode, response.Error.Message)
	}

	mlog.Info("Mattermost configuration complete")

	return nil
}

func checkDNS(ctx context.Context, url string) error {
	for {
		timeout := time.Duration(2 * time.Second)
		_, err := net.DialTimeout("tcp", url, timeout)
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s to become reachable", url)
		case <-time.After(10 * time.Second):
		}
	}
}

func checkMMPing(ctx context.Context, client *mattermostModel.Client4) error {
	for {
		status, response := client.GetPing()
		if response.StatusCode == 200 && status == "OK" {
			return nil
		}

		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for ok response")
		case <-time.After(10 * time.Second):
		}
	}
}

func (s *Server) makeSpinWickID(repoName string, prNumber int) string {
	domainName := s.Config.DNSNameTestServer
	spinWickID := strings.ToLower(fmt.Sprintf("%s-pr-%d", repoName, prNumber))
	// DNS names in MM cloud have a character limit. The number of characters in the domain - 64 will be how many we need to trim
	numCharactersToTrim := len(spinWickID+domainName) - 64
	if numCharactersToTrim > 0 {
		// trim the last numCharactersToTrim characters off of the repoName and overwrite spinWickID
		spinWickID = strings.ToLower(fmt.Sprintf("%s-pr-%d", repoName[:(len(repoName)-numCharactersToTrim)], prNumber))
	}
	return spinWickID
}

func (s *Server) isSpinWickLabel(label string) bool {
	return label == s.Config.SetupSpinWick || label == s.Config.SetupSpinWickHA
}

func (s *Server) isSpinWickLabelInLabels(labels []string) bool {
	for _, label := range labels {
		if s.isSpinWickLabel(label) {
			return true
		}
	}

	return false
}

func (s *Server) isSpinWickHALabel(labels []string) bool {
	for _, label := range labels {
		if label == s.Config.SetupSpinWickHA {
			return true
		}
	}
	return false
}

func (s *Server) removeCommentsWithSpecificMessages(comments []*github.IssueComment, serverMessages []string, pr *model.PullRequest) {
	mlog.Info("Removing old spinwick MatterWick comments")
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username {
			for _, message := range serverMessages {
				if strings.Contains(*comment.Body, message) {
					mlog.Info("Removing old spinwick comment with ID", mlog.Int64("ID", *comment.ID))
					_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
					if err != nil {
						mlog.Error("Unable to remove old spinwick MatterWick comment", mlog.Err(err))
					}
					break
				}
			}
		}
	}
}

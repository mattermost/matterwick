// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/google/go-github/v32/github"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	mattermostModel "github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/matterwick/internal/cloudtools"
	"github.com/mattermost/matterwick/internal/cws"
	"github.com/mattermost/matterwick/internal/spinwick"
	"github.com/mattermost/matterwick/model"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	// K8s packages for CWS
	"github.com/mattermost/mattermost-cloud/k8s"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	cwsRepoName          = "customer-web-server"
	cwsImage             = "mattermost/cws-test"
	cwsDeploymentName    = "cws-test"
	cwsSecretName        = "customer-web-server-secret"
	mattermostEEImage    = "mattermostdevelopment/mm-ee-test"
	mattermostTeamImage  = "mattermostdevelopment/mm-te-test"
	mattermostWebAppRepo = "mattermost-webapp"
	mattermostServerRepo = "mattermost"

	defaultMultiTenantAnnotation = "multi-tenant"
)

func (s *Server) handleCreateSpinWick(pr *model.PullRequest, size string, withLicense, withCloudInfra bool) {
	logger := s.Logger.WithFields(logrus.Fields{"repo_name": pr.RepoName, "pr": pr.Number})
	if pr.State == "closed" {
		logger.Info("PR is closed/merged, will not create a test server")
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "PR is closed/merged not creating a SpinWick Test server")
		return
	}

	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}
	if pr.RepoName == cwsRepoName {
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Creating a CWS SpinWick test server")
		request = s.createCWSSpinWick(pr, logger)
	} else if withCloudInfra {
		s.sendGitHubComment(
			pr.RepoOwner,
			pr.RepoName,
			pr.Number,
			"Creating a new SpinWick test cloud server with CWS using Mattermost Cloud.",
		)
		request = s.createCloudSpinWickWithCWS(pr, size, logger)
	} else {
		var commitMsg string
		if withLicense {
			commitMsg = "Creating a new HA SpinWick test server using Mattermost Cloud."
		} else {
			commitMsg = "Creating a new SpinWick test server using Mattermost Cloud."
		}
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, commitMsg)
		request = s.createSpinWick(pr, size, withLicense, nil, logger)
	}

	logger = logger.WithField("installation_id", request.InstallationID)

	if request.Error != nil {
		if request.Aborted {
			logger.WithError(request.Error).Warn("Aborted creation of SpinWick")
		} else {
			logger.WithError(request.Error).Error("Failed to create SpinWick")
		}
		comments, err := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
		if err != nil {
			logger.WithError(err).Error("Error getting comments")
		} else {
			s.removeOldComments(comments, pr, logger)
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
			s.logPrettyErrorToMattermost("[ SpinWick ] Creation Failed", pr, request.Error, additionalFields, logger)
		}
	}

}

// createCloudSpinwickWithCWS will use the defined CWSCloudInstance to create a new user/customer and
// instantiate a new MM cloud installation
func (s *Server) createCloudSpinWickWithCWS(pr *model.PullRequest, size string, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	uniqueID := s.makeSpinWickID(pr.RepoName, pr.Number)
	spinwickURL := fmt.Sprintf("https://%s.%s", uniqueID, s.Config.DNSNameTestServer)
	username := fmt.Sprintf("user-%s@example.mattermost.com", uniqueID)
	password := s.Config.CWSUserPassword

	// We try to login with an existing account and get the customer ID to create the installation
	// if there isn't an existing user, we create a new one
	var customerID string
	cwsClient := cws.NewClient(s.Config.CWSPublicAPIAddress, s.Config.CWSInternalAPIAddress, s.Config.CWSAPIKey)
	_, err := cwsClient.Login(username, password)
	if err != nil {
		response, err := cwsClient.SignUp(username, password)
		if err != nil {
			return request.WithError(errors.Wrap(err, "Error occurred whilst login or creating CWS user")).ShouldReportError()
		}
		err = cwsClient.VerifyUser(response.User.ID)
		if err != nil {
			return request.WithError(errors.Wrap(err, "Error occurred verifying the new CWS user")).ShouldReportError()
		}
		customerID = response.Customer.ID
	} else {
		customers, err := cwsClient.GetMyCustomers()
		if err != nil {
			return request.WithError(errors.Wrap(err, "Error occurred whilst login or creating CWS user")).ShouldReportError()
		}
		if len(customers) < 1 {
			return request.WithError(errors.Wrap(err, "Error occurred whilst login or creating CWS user")).ShouldReportError()
		}
		customerID = customers[0].ID
	}

	// Check for existing installations so we can abort the creation process if it exists
	installation, err := s.getActiveInstallationUsingCWS(cwsClient)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error trying to get existing installations")).ShouldReportError()
	}
	if installation != nil {
		return request.WithInstallationID(installation.ID).
			WithError(fmt.Errorf("Already found a installation belonging to %s", customerID)).
			IntentionalAbort()
	}
	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}

	image := mattermostEEImage
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image, logger)
	if errImage != nil {
		return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	version := s.Builds.getInstallationVersion(prNew)
	createInstallationRequest := &cws.CreateInstallationRequest{
		CustomerID:             customerID,
		RequestedWorkspaceName: uniqueID,
		Version:                version,
		Image:                  image,
		GroupID:                s.Config.CWSSpinwickGroupID,
		APILock:                false,
	}
	createResponse, err := cwsClient.CreateInstallation(createInstallationRequest)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error occurred whilst creating installation")).ShouldReportError()
	}
	request.InstallationID = createResponse.InstallationID
	s.waitForInstallationStable(ctx, pr, request, logger)
	if request.Error != nil {
		return request.WithError(errors.Wrap(request.Error, "error waiting for installation to become stable"))
	}

	userTable := fmt.Sprintf("| Account Type | Username | Password |\n|---|---|---|\n| Admin | %s | %s |", username, password)
	msg := fmt.Sprintf("Mattermost test server with CWS created! :tada:\n\nAccess here: %s\n\n%s", spinwickURL, userTable)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)
	return request
}

func (s *Server) createCWSSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

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
	image := cwsImage

	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}

	prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image, logger)
	if errImage != nil {
		return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	version = s.Builds.getInstallationVersion(prNew)

	deployment := Deployment{
		Namespace:      namespace.GetName(),
		ImageTag:       version,
		DeployFilePath: "/tmp/cws_deployment" + namespace.GetName() + ".yaml",
		Environment:    s.Config.CWS,
	}

	deployment.Environment.CWSSplitServerID = namespace.GetName()

	template, err := template.ParseFiles("/matterwick/templates/cws/cws_deployment.tmpl")
	if err != nil {
		logger.WithError(err).Error("Error loading deployment template ")
	}

	file, err := os.Create(deployment.DeployFilePath)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error creating deployment file")).ShouldReportError()
	}

	err = template.Execute(file, deployment)
	if err != nil {
		logger.WithError(err).Error("Error executing template ")
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

	logger.Info("Deployment created successfully. Cleanup complete")

	lbURL, _ := waitForIPAssignment(kc, deployment.Namespace, logger)

	headers := map[string]string{
		"x-api-key": s.Config.AWSAPIKey,
	}
	cloudClient := cloudModel.NewClientWithHeaders(s.Config.ProvisionerServer, headers)
	_, err = cloudClient.CreateWebhook(&cloudModel.CreateWebhookRequest{
		// We use the namespace as the owner so it's easily fetched later
		OwnerID: namespace.GetName(),
		URL:     fmt.Sprintf("http://cws-test-service.%s:8077/api/v1/internal/webhook", namespace.GetName()),
	})

	if err != nil {
		logger.WithError(err).Error("Unable to create webhook")
		return request.WithError(errors.Wrap(err, "Error creating provisioner webhook")).ShouldReportError()
	}

	cwsClient := cws.NewClient(s.Config.CWSPublicAPIAddress, s.Config.CWSInternalAPIAddress, s.Config.CWSAPIKey)

	secret, err := cwsClient.RegisterStripeWebhook(fmt.Sprintf("http://%s", lbURL), namespace.GetName())
	if err != nil {
		logger.WithError(err).Error("Unable to register stripe webhook")
		return request.WithError(errors.Wrap(err, "Error registering stripe webhook")).ShouldReportError()
	}

	base64lbURL := base64.StdEncoding.EncodeToString([]byte("http://" + lbURL))
	// Update the SiteURL now that we have it
	_, err = kc.Clientset.CoreV1().Secrets(namespaceName).Patch(
		ctx,
		cwsSecretName,
		types.JSONPatchType,
		[]byte(`[{"op": "replace", "path": "/data/CWS_SITEURL", "value": "`+base64lbURL+`"}, {"op": "replace", "path": "/data/STRIPE_WEBHOOK_SIGNATURE_SECRET", "value": "`+secret+`"}]`),
		metav1.PatchOptions{},
	)
	if err != nil {
		logger.WithError(err).Error("Unable to update CWS_SITEURL or STRIPE_WEBHOOK_SIGNATURE_SECRET secret")
	} else {
		// patch the deployment to force new pods that will be aware of the new secrets.
		_, err := kc.Clientset.AppsV1().Deployments(namespaceName).Patch(
			ctx,
			cwsDeploymentName,
			types.JSONPatchType,
			[]byte(`[{"op":"add","path":"/spec/template/metadata/labels/date","value":"`+time.Now().Format(time.RFC3339)+`"}]`),
			metav1.PatchOptions{},
		)
		if err != nil {
			logger.WithError(err).Error("Unable to refresh the deployment")
		}
	}

	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	commentsToDelete := []string{"Creating a SpinWick test CWS", "Spinwick Kubernetes namespace"}
	if errComments != nil {
		logger.WithError(err).Error("pr_error")
	} else {
		s.removeCommentsWithSpecificMessages(comments, commentsToDelete, pr, logger)
	}

	spinwickURL := fmt.Sprintf("http://%s", lbURL)
	msg := fmt.Sprintf("CWS test server created! :tada:\n\nAccess here: %s\n\nSplit individual target: %s", spinwickURL, deployment.Environment.CWSSplitServerID)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	request.InstallationID = deployment.Namespace
	return request
}

// createSpinwick creates a SpinWick with the following behavior:
// - no cloud installation found = installation is created
// - cloud installation found = actual ID string and no error
// - any errors = error is returned
func (s *Server) createSpinWick(pr *model.PullRequest, size string, withLicense bool, envVars cloudModel.EnvVarMap, logger logrus.FieldLogger) *spinwick.Request {
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
		logger.WithError(err).Error("pr_error")
	} else {
		s.removeCommentsWithSpecificMessages(comments, serverNewCommitMessages, pr, logger)
	}

	logger.Info("No SpinWick found for this PR. Creating a new one.")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	// set the version to master
	version := "master"
	image := mattermostEEImage

	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}
	// if is server or webapp then set version to the PR git commit hash
	if pr.RepoName == mattermostWebAppRepo {
		logger.Info("Waiting for docker image to set up SpinWick")

		// Waiting for Enterprise Image
		prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image, logger)
		if errImage != nil {
			return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
		}

		version = s.Builds.getInstallationVersion(prNew)
	} else if pr.RepoName == mattermostServerRepo {
		logger.Info("Waiting for docker image to set up SpinWick")

		ctxEnterprise, cancelEnterprise := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancelEnterprise()
		// Waiting for Enterprise Image
		prNew, errImage := s.Builds.waitForImage(ctxEnterprise, s, reg, pr, image, logger)
		if errImage != nil {
			if withLicense {
				s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Enterprise Edition Image not available in the 30 minutes timeframe.\nPlease check if the EE Pipeline was triggered and if not please trigger and re-add the `Setup HA Cloud Test Server` again.")
				return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting. Check if EE pipeline ran")).IntentionalAbort()
			}

			logger.WithField("sha", pr.Sha).Warn("Did not find the EE image, fallback to TE")
			s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Enterprise Edition Image not available in the 30 minutes timeframe, checking the Team Edition Image and if available will use that.")
			//fallback to TE
			image = mattermostTeamImage
			ctxTeam, cancelTeam := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancelTeam()
			prNew, errImage = s.Builds.waitForImage(ctxTeam, s, reg, pr, image, logger)
			if errImage != nil {
				logger.WithField("sha", pr.Sha).Warn("Did not find TE image")
				return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
			}
		}

		version = s.Builds.getInstallationVersion(prNew)
	}

	logger.Info("Provisioning Server - Installation request")

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
		OwnerID:     ownerID,
		Version:     version,
		Image:       image,
		DNS:         fmt.Sprintf("%s.%s", ownerID, s.Config.DNSNameTestServer),
		Size:        size,
		Affinity:    "multitenant",
		Database:    cloudModel.InstallationDatabaseMultiTenantRDSPostgresPGBouncer,
		Filestore:   cloudModel.InstallationFilestoreBifrost,
		Annotations: []string{defaultMultiTenantAnnotation},
	}
	if withLicense {
		installationRequest.License = s.Config.SpinWickHALicense
	}
	if envVars != nil && len(envVars) > 0 {
		installationRequest.MattermostEnv = envVars
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
	logger = logger.WithField("installation_id", request.InstallationID)
	logger.Info("Provisioner Server - installation request")

	wait := 1200
	logger.Info("Waiting %d seconds for mattermost installation to become stable")
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	s.waitForInstallationStable(ctx, pr, request, logger)
	if request.Error != nil {
		return request.WithError(errors.Wrap(request.Error, "error waiting for installation to become stable"))
	}

	spinwickURL := fmt.Sprintf("https://%s.%s", s.makeSpinWickID(pr.RepoName, pr.Number), s.Config.DNSNameTestServer)
	err = s.initializeMattermostTestServer(spinwickURL, pr.Number, logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "failed to initialize the Installation")).ShouldReportError()
	}
	userTable := "| Account Type | Username | Password |\n|---|---|---|\n| Admin | sysadmin | Sys@dmin123 |\n| User | user-1 | User-1@123 |"
	msg := fmt.Sprintf("Mattermost test server created! :tada:\n\nAccess here: %s\n\n%s", spinwickURL, userTable)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	return request
}

func (s *Server) handleUpdateSpinWick(pr *model.PullRequest, withLicense, withCloudInfra bool) {
	logger := s.Logger.WithFields(logrus.Fields{"repo_name": pr.RepoName, "pr": pr.Number})

	// other repos we are not updating
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	if pr.RepoName == cwsRepoName {
		request = s.updateKubeSpinWick(pr, logger)
	} else {
		request = s.updateSpinWick(pr, withLicense, withCloudInfra, logger)
	}

	logger = logger.WithField("installation_id", request.InstallationID)

	if request.Error != nil {
		if request.Aborted {
			logger.WithError(request.Error).Warn("Aborted update of SpinWick")
		} else {
			logger.WithError(request.Error).Error("Failed to update SpinWick")
		}
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, s.Config.SetupSpinmintFailedMessage)
		if request.ReportError {
			additionalFields := map[string]string{
				"Installation ID": request.InstallationID,
			}
			s.logPrettyErrorToMattermost("[ SpinWick ] Update Failed", pr, request.Error, additionalFields, logger)
		}
	}
}

func (s *Server) updateKubeSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

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

	// Remove old message to reduce the amount of similar messages and avoid confusion
	serverNewCommitMessages := []string{
		"New commit detected.",
	}
	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	if errComments != nil {
		logger.WithError(err).Error("pr_error")
	} else {
		s.removeCommentsWithSpecificMessages(comments, serverNewCommitMessages, pr, logger)
	}
	// Now that we know this namespace exists, notify via comment that we are attempting to upgrade the deployment
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "New commit detected. SpinWick will upgrade if the updated docker image is available.")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	version := ""
	image := cwsImage

	reg, errDocker := s.Builds.dockerRegistryClient(s)
	if errDocker != nil {
		return request.WithError(errors.Wrap(errDocker, "unable to get docker registry client")).ShouldReportError()
	}

	prNew, errImage := s.Builds.waitForImage(ctx, s, reg, pr, image, logger)
	if errImage != nil {
		return request.WithError(errors.Wrap(errImage, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	version = s.Builds.getInstallationVersion(prNew)

	deployClient := kc.Clientset.AppsV1().Deployments(namespaceName)
	deployment, err := deployClient.Get(context.Background(), "cws-test", metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		logger.Warn("Attempted to update a deployment that does not exist")
		return request.WithError(errors.Wrap(err, "Attempted to update a deployment that does not exist")).ShouldReportError()
	}

	for idx := range deployment.Spec.Template.Spec.Containers {
		deployment.Spec.Template.Spec.Containers[idx].Image = image + ":" + version
	}

	for idx := range deployment.Spec.Template.Spec.InitContainers {
		deployment.Spec.Template.Spec.InitContainers[idx].Image = image + ":" + version
	}

	_, err = deployClient.Update(context.Background(), deployment, metav1.UpdateOptions{})

	if err != nil {
		return request.WithError(errors.Wrap(err, "failed while updating deployment with latest image")).ShouldReportError()
	}

	// Remove old message to reduce the amount of similar messages and avoid confusion
	if errComments == nil {
		serverUpdateMessage := []string{
			"CWS test server updated",
		}
		s.removeCommentsWithSpecificMessages(comments, serverUpdateMessage, pr, logger)
	}

	lbURL, _ := waitForIPAssignment(kc, namespaceName, logger)
	spinwickURL := fmt.Sprintf("http://%s", lbURL)
	msg := fmt.Sprintf("CWS test server updated with git commit `%s`.\n\nAccess here: %s", pr.Sha, spinwickURL)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	return request
}

// updateSpinWick updates a SpinWick with the following behavior:
// - no cloud installation found = error is returned
// - cloud installation found and updated = actual ID string and no error
// - any errors = error is returned
func (s *Server) updateSpinWick(pr *model.PullRequest, withLicense, withCloudInfra bool, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	var ownerID string
	var err error
	if withCloudInfra {
		ownerID, err = s.getCustomerIDFromCWS(pr.RepoName, pr.Number)
		if err != nil {
			return request.WithError(errors.Wrap(err, "error getting the owner id from CWS")).ShouldReportError()
		}
	} else {
		ownerID = s.makeSpinWickID(pr.RepoName, pr.Number)
	}

	installationID, image, err := cloudtools.GetInstallationIDFromOwnerID(s.Config.ProvisionerServer, s.Config.AWSAPIKey, ownerID)
	if err != nil {
		return request.WithError(err).ShouldReportError()
	}
	if installationID == "" {
		return request.WithError(fmt.Errorf("no installation found with owner %s", ownerID)).ShouldReportError()
	}
	request.InstallationID = installationID

	logger = logger.WithField("sha", pr.Sha)
	logger.Info("Sleeping a bit to wait for the build process to start")
	time.Sleep(60 * time.Second)

	// Remove old message to reduce the amount of similar messages and avoid confusion
	serverNewCommitMessages := []string{
		"New commit detected.",
	}
	comments, errComments := s.getComments(pr.RepoOwner, pr.RepoName, pr.Number)
	if errComments != nil {
		logger.WithError(err).Error("pr_error")
	} else {
		s.removeCommentsWithSpecificMessages(comments, serverNewCommitMessages, pr, logger)
	}
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "New commit detected. SpinWick will upgrade if the updated docker image is available.")

	reg, err := s.Builds.dockerRegistryClient(s)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get docker registry client")).ShouldReportError()
	}

	logger.Info("Waiting for docker image to update SpinWick")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pr, err = s.Builds.waitForImage(ctx, s, reg, pr, image, logger)
	if err != nil {
		return request.WithError(errors.Wrap(err, "error waiting for the docker image. Aborting")).IntentionalAbort()
	}

	installationVersion := s.Builds.getInstallationVersion(pr)

	upgradeRequest := &cloudModel.PatchInstallationRequest{
		Version: &installationVersion,
		Image:   &image,
	}
	if withLicense && !withCloudInfra {
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

	logger.Info("Provisioning Server - Upgrade request")

	_, err = cloudClient.UpdateInstallation(request.InstallationID, upgradeRequest)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to make upgrade request to provisioning server")).ShouldReportError()
	}

	wait := 600
	logger.Infof("Waiting %d seconds for mattermost installation to become stable", wait)
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	s.waitForInstallationStable(ctx, pr, request, logger)
	if request.Error != nil {
		return request.WithError(errors.Wrap(request.Error, "error waiting for installation to become stable"))
	}

	// Remove old message to reduce the amount of similar messages and avoid confusion
	if errComments == nil {
		serverUpdateMessage := []string{
			"Mattermost test server updated",
		}
		s.removeCommentsWithSpecificMessages(comments, serverUpdateMessage, pr, logger)
	}

	mmURL := fmt.Sprintf("https://%s.%s", s.makeSpinWickID(pr.RepoName, pr.Number), s.Config.DNSNameTestServer)
	msg := fmt.Sprintf("Mattermost test server updated with git commit `%s`.\n\nAccess here: %s", pr.Sha, mmURL)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, msg)

	return request
}

func (s *Server) handleDestroySpinWick(pr *model.PullRequest, withCloud bool) {
	logger := s.Logger.WithFields(logrus.Fields{"repo_name": pr.RepoName, "pr": pr.Number})

	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	if pr.RepoName == cwsRepoName {
		request = s.destroyKubeSpinWick(pr, logger)
	} else if withCloud {
		request = s.destroyCloudSpinWickWithCWS(pr, logger)
	} else {
		request = s.destroySpinWick(pr, logger)
	}

	logger = logger.WithField("installation_id", request.InstallationID)

	if request.Error != nil {
		if request.Aborted {
			logger.WithError(request.Error).Warn("Aborted deletion of SpinWick")
		} else {
			logger.WithError(request.Error).Error("Failed to delete SpinWick")
		}
		if request.ReportError {
			additionalFields := map[string]string{
				"Installation ID": request.InstallationID,
			}
			s.logPrettyErrorToMattermost("[ SpinWick ] Destroy Failed", pr, request.Error, additionalFields, logger)
		}
	}
}

func (s *Server) destroyKubeSpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	logger.Info("Received request to destroy kubernetes namespace")
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

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
		logger.WithError(err).Error("Failed while deleting namespace")
		request.Error = err
		return request
	}
	request.InstallationID = namespaceName
	logger.Infof("Kube namespace %s has been destroyed", namespaceName)

	headers := map[string]string{
		"x-api-key": s.Config.AWSAPIKey,
	}
	cloudClient := cloudModel.NewClientWithHeaders(s.Config.ProvisionerServer, headers)
	webhooks, err := cloudClient.GetWebhooks(&cloudModel.GetWebhooksRequest{
		OwnerID: namespaceName,
	})
	if err != nil {
		logger.WithError(err).Error("Failed to get provisioner webhooks for spinwick")
		request.Error = err
		return request
	}

	for _, webhook := range webhooks {
		err = cloudClient.DeleteWebhook(webhook.ID)
		if err != nil {
			logger.WithError(err).Error("Failed to delete provisioner webhook")
			request.Error = err
			return request
		}
	}

	cwsClient := cws.NewClient(s.Config.CWSPublicAPIAddress, s.Config.CWSInternalAPIAddress, s.Config.CWSAPIKey)
	err = cwsClient.DeleteStripeWebhook(namespaceName)
	if err != nil {
		logger.WithError(err).Error("Failed to delete stripe webhook")
		request.Error = err
		return request
	}

	// Old comments created by MatterWick user will be deleted here.
	s.commentLock.Lock()
	defer s.commentLock.Unlock()
	comments, _, err := newGithubClient(s.Config.GithubAccessToken).Issues.ListComments(context.Background(), pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get list of old comments")).ShouldReportError()
	}
	s.removeOldComments(comments, pr, logger)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, "Spinwick CWS test server has been destroyed.")
	return request
}

// destroyCloudSpinWickWithCWS destroys the Spinwick installation for the passed PR
// using CWS so we can get rid of the installation but also for all the intermediate
// metadata
func (s *Server) destroyCloudSpinWickWithCWS(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
	request := &spinwick.Request{
		InstallationID: "n/a",
		Error:          nil,
		ReportError:    false,
		Aborted:        false,
	}

	uniqueID := s.makeSpinWickID(pr.RepoName, pr.Number)
	username := fmt.Sprintf("user-%s@example.mattermost.com", uniqueID)
	password := s.Config.CWSUserPassword

	cwsClient := cws.NewClient(s.Config.CWSPublicAPIAddress, s.Config.CWSInternalAPIAddress, s.Config.CWSAPIKey)
	_, err := cwsClient.Login(username, password)
	if err != nil {
		return request.WithError(errors.Wrap(err, "error trying to login in the public CWS server")).ShouldReportError()
	}

	installation, err := s.getActiveInstallationUsingCWS(cwsClient)
	if err != nil {
		return request.WithError(errors.Wrap(err, "Error trying to get existing installations")).ShouldReportError()
	}
	if installation == nil {
		return request.WithError(errors.New("there isn't any installation for that PR")).ShouldReportError()
	}

	request.InstallationID = installation.ID

	logger.WithField("installation_id", installation.ID).Info("Found installation. Starting deletion...")
	err = cwsClient.DeleteInstallation(installation.ID)
	if err != nil {
		return request.WithInstallationID(installation.ID).
			WithError(errors.Wrap(err, "error trying to initiate the installation deletion for the PR ")).
			ShouldReportError()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s.waitForInstallationIsDeleted(ctx, pr, request, logger)

	// Old comments created by MatterWick user will be deleted here.
	s.commentLock.Lock()
	defer s.commentLock.Unlock()
	comments, _, err := newGithubClient(s.Config.GithubAccessToken).Issues.ListComments(context.Background(), pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		return request.WithError(errors.Wrap(err, "unable to get list of old comments")).ShouldReportError()
	}
	s.removeOldComments(comments, pr, logger)
	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, s.Config.DestroyedSpinmintMessage)
	return request
}

// destroySpinwick destroys a SpinWick with the following behavior:
// - no cloud installation found = empty ID string and no error
// - cloud installation found and deleted = actual ID string and no error
// - any errors = error is returned
func (s *Server) destroySpinWick(pr *model.PullRequest, logger logrus.FieldLogger) *spinwick.Request {
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

	logger.WithField("installation_id", request.InstallationID).Info("Destroying SpinWick")

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
	s.removeOldComments(comments, pr, logger)

	s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, s.Config.DestroyedSpinmintMessage)

	return request
}

func (s *Server) waitForInstallationStable(ctx context.Context, pr *model.PullRequest, request *spinwick.Request, logger logrus.FieldLogger) {
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

			logger.WithFields(logrus.Fields{
				"installation_id": request.InstallationID,
				"state":           payload.NewState,
			}).Info("Installation changed state")

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
				s.sendGitHubComment(
					pr.RepoOwner,
					pr.RepoName,
					pr.Number,
					"No Kubernetes clusters available at the moment, please contact the Mattermost Cloud Team or wait a bit.")
				request.WithError(errors.New("no k8s clusters available")).IntentionalAbort()
				return
			}
		}
	}
}

func (s *Server) waitForInstallationIsDeleted(ctx context.Context, pr *model.PullRequest, request *spinwick.Request, logger logrus.FieldLogger) {
	channel, err := s.requestCloudWebhookChannel(request.InstallationID)
	if err != nil {
		request.WithError(err).ShouldReportError()
		return
	}
	defer s.removeCloudWebhookChannel(request.InstallationID)

	for {
		select {
		case <-ctx.Done():
			request.WithError(errors.New("timed out waiting for the mattermost installation to be deleted")).ShouldReportError()
			return
		case payload := <-channel:
			if payload.ID != request.InstallationID {
				continue
			}

			logger.WithFields(logrus.Fields{
				"installation_id": request.InstallationID,
				"state":           payload.NewState,
			}).Info("Installation changed state")

			switch payload.NewState {
			case cloudModel.InstallationStateDeleted:
				return
			case cloudModel.InstallationStateDeletionFailed:
				request.WithError(errors.New("the installation deletion failed")).ShouldReportError()
				return
			}
		}
	}
}

func (s *Server) initializeMattermostTestServer(mmURL string, prNumber int, logger logrus.FieldLogger) error {
	logger.Info("Initializing Mattermost installation")

	wait := 600
	logger.Infof("Waiting up to %d seconds for DNS to propagate", wait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	mmHost, _ := url.Parse(mmURL)
	err := checkDNS(ctx, fmt.Sprintf("%s:443", mmHost.Host))
	if err != nil {
		return errors.Wrap(err, "timed out waiting for DNS to propagate for installation")
	}

	client := mattermostModel.NewAPIv4Client(mmURL)

	// check if Mattermost is available
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
	_, _, err = client.CreateUser(user)
	if err != nil {
		return errors.Wrap(err, "failed to create initial mattermost user")
	}
	client.Logout()

	userLogged, _, err := client.Login("sysadmin", "Sys@dmin123")
	if err != nil {
		return errors.Wrap(err, "failed to log in with initial mattermost user")
	}

	teamName := fmt.Sprintf("pr%d", prNumber)
	team := &mattermostModel.Team{
		Name:        teamName,
		DisplayName: teamName,
		Type:        "O",
	}
	firstTeam, _, err := client.CreateTeam(team)
	if err != nil {
		return errors.Wrap(err, "failed to log in with initial team")
	}

	_, _, err = client.AddTeamMember(firstTeam.Id, userLogged.Id)
	if err != nil {
		return errors.Wrap(err, "failed adding admin user to initial team")
	}

	testUser := &mattermostModel.User{
		Username: "user-1",
		Email:    "user-1@example.mattermost.com",
		Password: "User-1@123",
	}
	testUser, _, err = client.CreateUser(testUser)
	if err != nil {
		return errors.Wrap(err, "failed to create standard test user")
	}
	_, _, err = client.AddTeamMember(firstTeam.Id, testUser.Id)
	if err != nil {
		return errors.Wrap(err, "failed adding standard test user to initial team")
	}

	logger.Info("Mattermost configuration complete")

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
		_, response, _ := client.GetPing()
		if response.StatusCode == http.StatusOK {
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

func (s *Server) getCustomerIDFromCWS(repoName string, prNumber int) (string, error) {
	cwsClient := cws.NewClient(s.Config.CWSPublicAPIAddress, s.Config.CWSInternalAPIAddress, s.Config.CWSAPIKey)
	uniqueID := s.makeSpinWickID(repoName, prNumber)
	_, err := cwsClient.Login(
		fmt.Sprintf("user-%s@example.mattermost.com", uniqueID),
		s.Config.CWSUserPassword,
	)
	if err != nil {
		return "", err
	}
	customers, err := cwsClient.GetMyCustomers()
	if err != nil {
		return "", err
	}
	if len(customers) < 1 {
		return "", errors.New("user don't have any customer")
	}
	return fmt.Sprintf("cws-%s", customers[0].ID), nil
}

func (s *Server) isSpinWickLabel(label string) bool {
	return label == s.Config.SetupSpinWick || label == s.Config.SetupSpinWickHA || label == s.Config.SetupSpinWickWithCWS
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

func (s *Server) isSpinWickCloudWithCWSLabel(labels []string) bool {
	for _, label := range labels {
		if label == s.Config.SetupSpinWickWithCWS {
			return true
		}
	}
	return false
}

func (s *Server) removeCommentsWithSpecificMessages(comments []*github.IssueComment, serverMessages []string, pr *model.PullRequest, logger logrus.FieldLogger) {
	logger.Info("Removing old spinwick MatterWick comments")
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username {
			for _, message := range serverMessages {
				if strings.Contains(*comment.Body, message) {
					logger.WithField("comment_id", *comment.ID).Info("Removing old spinwick comment with ID")
					_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
					if err != nil {
						logger.WithError(err).Error("Unable to remove old spinwick MatterWick comment")
					}
					break
				}
			}
		}
	}
}

func (s *Server) getActiveInstallationUsingCWS(client *cws.Client) (*cws.Installation, error) {
	installations, err := client.GetInstallations()
	if err != nil {
		return nil, errors.Wrap(err, "Error trying to get existing installations")
	}
	if len(installations) < 1 {
		return nil, nil
	}

	for _, installation := range installations {
		switch installation.State {
		case cloudModel.InstallationStateDeletionRequested,
			cloudModel.InstallationStateDeletionInProgress,
			cloudModel.InstallationStateDeleted,
			cloudModel.InstallationStateCreationFailed:
			continue
		default:
			return installation, nil
		}
	}

	return nil, nil
}

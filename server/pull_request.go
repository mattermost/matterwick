// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"strings"

	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"

	"github.com/google/go-github/v32/github"
)

func (s *Server) handlePullRequestEvent(event *github.PullRequestEvent) {
	repoName := event.GetRepo().GetName()
	prNumber := event.GetNumber()
	label := event.GetLabel().GetName()

	logger := s.Logger.WithFields(logrus.Fields{"repo": repoName, "pr": prNumber, "action": event.GetAction()})
	logger.Info("PR-Event")

	pr, err := s.GetPullRequestFromGithub(event.PullRequest)
	if err != nil {
		logger.WithError(err).Error("Unable to get PR from GitHub")
		return
	}

	spinwick := model.NewSpinwick(pr.RepoName, pr.Number, s.Config.DNSNameTestServer)

	switch event.GetAction() {
	case "opened":
		logger.Info("PR opened")
	case "reopened":
		logger.Info("PR reopened")
	case "labeled":
		if event.Label == nil {
			logger.Error("Label event received, but label object was empty")
			return
		}

		if s.isSpinWickLabel(label) {
			logger.WithField("label", label).Info("PR received SpinWick label")
			switch *event.Label.Name {
			case s.Config.SetupSpinWick:
				s.handleCreateSpinWick(pr, "miniSingleton", false, false, s.getEnvMap(spinwick.RepeatableID))
			case s.Config.SetupSpinWickHA:
				s.handleCreateSpinWick(pr, "miniHA", true, false, s.getEnvMap(spinwick.RepeatableID))
			case s.Config.SetupSpinWickWithCWS:
				s.handleCreateSpinWick(pr, "miniSingleton", true, true, s.getEnvMap(spinwick.RepeatableID))
			default:
				logger.WithField("label", label).Error("Failed to determine sizing on SpinWick label")
			}
		}
	case "unlabeled":
		if event.Label == nil {
			logger.Error("Unlabel event received, but label object was empty")
			return
		}
		if s.isSpinWickLabel(label) {
			logger.WithField("label", label).Info("PR SpinWick label was removed")
			switch *event.Label.Name {
			case s.Config.SetupSpinWickWithCWS:
				s.handleDestroySpinWick(pr, true)
			case s.Config.SetupSpinWickHA, s.Config.SetupSpinWick:
				s.handleDestroySpinWick(pr, false)
			}
		}
	case "synchronize":
		logger.Info("PR has a new commit")

		s.handleSynchronizeSpinwick(pr, spinwick.RepeatableID, false)
	case "closed":
		logger.Info("PR was closed")
		if s.isSpinWickLabelInLabels(pr.Labels) {
			if s.isSpinWickCloudWithCWSLabel(pr.Labels) {
				s.handleDestroySpinWick(pr, true)
			} else {
				s.handleDestroySpinWick(pr, false)
			}
		}
	}
}

func (s *Server) handleSynchronizeSpinwick(pr *model.PullRequest, spinwickID string, noBuildChanges bool) {
	if s.isSpinWickLabelInLabels(pr.Labels) {
		if s.isSpinWickHALabel(pr.Labels) {
			s.handleUpdateSpinWick(pr, true, false, noBuildChanges, s.getEnvMap(spinwickID))
		} else if s.isSpinWickCloudWithCWSLabel(pr.Labels) {
			s.handleUpdateSpinWick(pr, true, true, noBuildChanges, s.getEnvMap(spinwickID))
		} else {
			s.handleUpdateSpinWick(pr, false, false, noBuildChanges, s.getEnvMap(spinwickID))
		}
	}
}

func (s *Server) removeOldComments(comments []*github.IssueComment, pr *model.PullRequest, logger logrus.FieldLogger) {
	serverMessages := []string{
		s.Config.SetupSpinmintFailedMessage,
		"Spinmint test server created",
		"Spinmint upgrade test server created",
		"New commit detected",
		"Error during the request to upgrade",
		"Error doing the upgrade process",
		"Timed out waiting",
		"Mattermost test server created!",
		"Mattermost test server updated!",
		"Failed to create mattermost installation",
		"Creating a new SpinWick test server using Mattermost Cloud.",
		"Please wait while a new kubernetes cluster is created for your SpinWick",
		"Mattermost test server updated with git commit",
		"Enterprise Edition Image not available",
		"CWS test server created!",
		"Creating a SpinWick test customer web server",
		"Spinwick CWS test server has been destroyed",
		"CWS test server updated",
		"Creating a new SpinWick test cloud server with CWS",
		"Mattermost test server with CWS created",
	}

	logger.Info("Removing old Matterwick comments")
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username {
			for _, message := range serverMessages {
				if strings.Contains(*comment.Body, message) {
					logger.Infof("Removing old comment with ID %d", *comment.ID)
					_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
					if err != nil {
						logger.WithError(err).Error("Unable to remove old MatterWick comment")
					}
					break
				}
			}
		}
	}
}

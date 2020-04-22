// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"strings"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/matterwick/model"

	"github.com/google/go-github/v28/github"
)

func (s *Server) handlePullRequestEvent(event *github.PullRequestEvent) {
	onwer := event.GetRepo().GetOwner().GetLogin()
	repoName := event.GetRepo().GetName()
	prNumber := event.GetNumber()
	label := event.GetLabel().GetName()

	mlog.Info("PR-Event", mlog.String("repo", repoName), mlog.Int("pr", prNumber), mlog.String("action", event.GetAction()))
	pr, err := s.GetPullRequestFromGithub(event.PullRequest)
	if err != nil {
		mlog.Error("Unable to get PR from GitHub", mlog.Int("pr", prNumber), mlog.Err(err))
		return
	}

	switch event.GetAction() {
	case "opened":
		mlog.Info("PR opened", mlog.String("repo", repoName), mlog.Int("pr", pr.Number))
	case "reopened":
		mlog.Info("PR reopened", mlog.String("repo", repoName), mlog.Int("pr", pr.Number))
	case "labeled":
		if event.Label == nil {
			mlog.Error("Label event received, but label object was empty")
			return
		}
		if s.isSpinWickLabel(label) {
			mlog.Info("PR received SpinWick label", mlog.String("repo", repoName), mlog.Int("pr", prNumber), mlog.String("label", label))
			switch *event.Label.Name {
			case s.Config.SetupSpinWick:
				s.sendGitHubComment(onwer, repoName, prNumber, "Creating a new SpinWick test server using Mattermost Cloud.")
				s.handleCreateSpinWick(pr, "miniSingleton", false)
			case s.Config.SetupSpinWickHA:
				s.sendGitHubComment(onwer, repoName, prNumber, "Creating a new HA SpinWick test server using Mattermost Cloud.")
				s.handleCreateSpinWick(pr, "miniHA", true)
			default:
				mlog.Error("Failed to determine sizing on SpinWick label", mlog.String("label", label))
			}
		}
	case "unlabeled":
		if event.Label == nil {
			mlog.Error("Unlabel event received, but label object was empty")
			return
		}
		if s.isSpinWickLabel(label) {
			mlog.Info("PR SpinWick label was removed", mlog.String("repo", repoName), mlog.Int("pr", prNumber), mlog.String("label", label))
			s.handleDestroySpinWick(pr)
		}
	case "synchronize":
		mlog.Info("PR has a new commit", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
		if s.isSpinWickLabelInLabels(pr.Labels) {
			mlog.Info("PR has a SpinWick label, starting upgrade", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
			if s.isSpinWickHALabel(pr.Labels) {
				s.handleUpdateSpinWick(pr, true)
			} else {
				s.handleUpdateSpinWick(pr, false)
			}
		}
	case "closed":
		mlog.Info("PR was closed", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
		if s.isSpinWickLabelInLabels(pr.Labels) {
			s.handleDestroySpinWick(pr)
		}
	}

}

func (s *Server) handlePRLabeled(pr *model.PullRequest, addedLabel string) {
	mlog.Info("New PR label detected", mlog.Int("pr", pr.Number), mlog.String("label", addedLabel))

	// Must be sure the comment is created before we let another request test
	s.commentLock.Lock()
	defer s.commentLock.Unlock()

	comments, _, err := newGithubClient(s.Config.GithubAccessToken).Issues.ListComments(context.Background(), pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		mlog.Error("Unable to list comments for PR", mlog.Int("pr", pr.Number), mlog.Err(err))
		return
	}

	// Old comment created by Mattermod user for test server deletion will be deleted here
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username &&
			strings.Contains(*comment.Body, s.Config.DestroyedSpinmintMessage) || strings.Contains(*comment.Body, s.Config.DestroyedExpirationSpinmintMessage) {
			mlog.Info("Removing old server deletion comment with ID", mlog.Int64("ID", *comment.ID))
			_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
			if err != nil {
				mlog.Error("Unable to remove old server deletion comment", mlog.Err(err))
			}
		}
	}

	for _, label := range s.Config.PrLabels {
		mlog.Info("looking for label", mlog.String("label", label.Label))
		finalMessage := strings.Replace(label.Message, "USERNAME", pr.Username, -1)
		if label.Label == addedLabel && !messageByUserContains(comments, s.Config.Username, finalMessage) {
			mlog.Info("Posted message for label on PR: ", mlog.String("label", label.Label), mlog.Int("pr", pr.Number))
			s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, finalMessage)
		}
	}

}

func (s *Server) removeOldComments(comments []*github.IssueComment, pr *model.PullRequest) {
	serverMessages := []string{s.Config.SetupSpinmintMessage,
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
		"Kubernetes cluster created",
		"Failed to create the k8s cluster",
		"Creating a new SpinWick test server using Mattermost Cloud.",
		"Please wait while a new kubernetes cluster is created for your SpinWick",
	}

	mlog.Info("Removing old Mattermod comments")
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username {
			for _, message := range serverMessages {
				if strings.Contains(*comment.Body, message) {
					mlog.Info("Removing old comment with ID", mlog.Int64("ID", *comment.ID))
					_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
					if err != nil {
						mlog.Error("Unable to remove old Mattermod comment", mlog.Err(err))
					}
					break
				}
			}
		}
	}
}

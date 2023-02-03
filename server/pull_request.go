// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"strings"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/matterwick/model"

	"github.com/google/go-github/v32/github"
)

func (s *Server) handlePullRequestEvent(event *github.PullRequestEvent) {
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
				s.handleCreateSpinWick(pr, "miniSingleton", false, false)
			case s.Config.SetupSpinWickHA:
				s.handleCreateSpinWick(pr, "miniHA", true, false)
			case s.Config.SetupSpinWickWithCWS:
				s.handleCreateSpinWick(pr, "miniSingleton", true, true)
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
			switch *event.Label.Name {
			case s.Config.SetupSpinWickWithCWS:
				s.handleDestroySpinWick(pr, true)
			case s.Config.SetupSpinWickHA, s.Config.SetupSpinWick:
				s.handleDestroySpinWick(pr, false)
			}
		}
	case "synchronize":
		mlog.Info("PR has a new commit", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
		if s.isSpinWickLabelInLabels(pr.Labels) {
			mlog.Info("PR has a SpinWick label, starting upgrade", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
			if s.isSpinWickHALabel(pr.Labels) {
				s.handleUpdateSpinWick(pr, true, false, false)
			} else if s.isSpinWickCloudWithCWSLabel(pr.Labels) {
				s.handleUpdateSpinWick(pr, true, true, false)
			} else {
				s.handleUpdateSpinWick(pr, false, false, false)
			}

			if pr.RepoName == mattermostWebAppRepo || pr.RepoName == mattermostServerRepo {
				mlog.Info("No SpinWick label found, checking for sister PR", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
				searchSisterPR := *pr
				searchSisterPR.RepoName = mattermostWebAppRepo
				if pr.RepoName == mattermostWebAppRepo {
					searchSisterPR.RepoName = mattermostServerRepo
				}
				sisterPR, _ := s.pullRequestWithBranchNameExists(&searchSisterPR)
				if sisterPR != nil {
					// We want to update the siser PR spinwick using the current PR sha
					sisterPR.Sha = pr.Sha
					mlog.Info("Sister PR found, starting upgrade", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
					if s.isSpinWickHALabel(sisterPR.Labels) {
						s.handleUpdateSpinWick(sisterPR, true, false, true)
					} else if s.isSpinWickCloudWithCWSLabel(sisterPR.Labels) {
						s.handleUpdateSpinWick(sisterPR, true, true, true)
					} else {
						s.handleUpdateSpinWick(sisterPR, false, false, true)
					}
				}
			}
		}
	case "closed":
		mlog.Info("PR was closed", mlog.String("repo", repoName), mlog.Int("pr", prNumber))
		if s.isSpinWickLabelInLabels(pr.Labels) {
			if s.isSpinWickCloudWithCWSLabel(pr.Labels) {
				s.handleDestroySpinWick(pr, true)
			} else {
				s.handleDestroySpinWick(pr, false)
			}
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

	// Old comment created by MatterWick user for test server deletion will be deleted here
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username &&
			strings.Contains(*comment.Body, s.Config.DestroyedSpinmintMessage) {
			mlog.Info("Removing old server deletion comment with ID", mlog.Int64("ID", *comment.ID))
			_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
			if err != nil {
				mlog.Error("Unable to remove old server deletion comment", mlog.Err(err))
			}
		}
	}
}

func (s *Server) removeOldComments(comments []*github.IssueComment, pr *model.PullRequest) {
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

	mlog.Info("Removing old Matterwick comments")
	for _, comment := range comments {
		if *comment.User.Login == s.Config.Username {
			for _, message := range serverMessages {
				if strings.Contains(*comment.Body, message) {
					mlog.Info("Removing old comment with ID", mlog.Int64("ID", *comment.ID))
					_, err := newGithubClient(s.Config.GithubAccessToken).Issues.DeleteComment(context.Background(), pr.RepoOwner, pr.RepoName, *comment.ID)
					if err != nil {
						mlog.Error("Unable to remove old MatterWick comment", mlog.Err(err))
					}
					break
				}
			}
		}
	}
}

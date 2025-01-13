// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mattermost/matterwick/model"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/google/go-github/v32/github"
	"golang.org/x/oauth2"
)

// PullRequestEventFromJSON parses json to a github.PullRequestEvent
func PullRequestEventFromJSON(data io.Reader) (*github.PullRequestEvent, error) {
	decoder := json.NewDecoder(data)
	var event github.PullRequestEvent
	if err := decoder.Decode(&event); err != nil {
		return nil, errors.Wrap(err, "failed to parse pull request event from JSON")
	}

	return &event, nil
}

// IssueCommentEventFromJSON parses json to a github.IssueCommentEvent
func IssueCommentEventFromJSON(data io.Reader) (*github.IssueCommentEvent, error) {
	decoder := json.NewDecoder(data)
	var event github.IssueCommentEvent
	if err := decoder.Decode(&event); err != nil {
		return nil, errors.Wrap(err, "failed to parse issue comment event from JSON")
	}

	return &event, nil
}

// PingEventFromJSON parses json to a github.PingEvent
func PingEventFromJSON(data io.Reader) (*github.PingEvent, error) {
	decoder := json.NewDecoder(data)
	var event github.PingEvent
	if err := decoder.Decode(&event); err != nil {
		return nil, errors.Wrap(err, "failed to parse ping event from JSON")
	}

	return &event, nil
}

func newGithubClient(token string) *github.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)

	return github.NewClient(tc)
}

// GetPullRequestFromGithub get updated pr info
func (s *Server) GetPullRequestFromGithub(pullRequest *github.PullRequest) (*model.PullRequest, error) {
	pr := &model.PullRequest{
		RepoOwner: *pullRequest.Base.Repo.Owner.Login,
		RepoName:  *pullRequest.Base.Repo.Name,
		Number:    *pullRequest.Number,
		Username:  *pullRequest.User.Login,
		FullName:  "",
		Ref:       *pullRequest.Head.Ref,
		Sha:       *pullRequest.Head.SHA,
		State:     *pullRequest.State,
		URL:       *pullRequest.URL,
		CreatedAt: pullRequest.GetCreatedAt(),
	}

	if pullRequest.Head.Repo != nil {
		pr.FullName = *pullRequest.Head.Repo.FullName
	}

	client := newGithubClient(s.Config.GithubAccessToken)

	labels, _, err := client.Issues.ListLabelsByIssue(context.Background(), pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		return nil, err
	}
	pr.Labels = labelsToStringArray(labels)

	return pr, nil
}

func (s *Server) getPullRequestFromIssue(issue *github.Issue, repo *github.Repository) (*github.PullRequest, error) {
	if !issue.IsPullRequest() {
		return nil, fmt.Errorf("issue is not a pull request")
	}

	client := newGithubClient(s.Config.GithubAccessToken)

	// Fetch the pull request
	pr, _, err := client.PullRequests.Get(context.Background(),
		repo.GetOwner().GetLogin(), repo.GetName(), issue.GetNumber())
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request: %w", err)
	}

	return pr, nil
}

func labelsToStringArray(labels []*github.Label) []string {
	out := make([]string, len(labels))

	for i, label := range labels {
		out[i] = *label.Name
	}

	return out
}

func (s *Server) sendGitHubComment(repoOwner, repoName string, number int, comment string) {
	logger := s.Logger.WithFields(logrus.Fields{"issue": number, "comment": comment})
	logger.Info("Sending GitHub comment")
	client := newGithubClient(s.Config.GithubAccessToken)
	_, _, err := client.Issues.CreateComment(context.Background(), repoOwner, repoName, number, &github.IssueComment{Body: &comment})
	if err != nil {
		logger.WithError(err).Error("Error commenting")
	}
}

func (s *Server) removeLabel(repoOwner, repoName string, number int, label string) {
	logger := s.Logger.WithFields(logrus.Fields{"issue": number, "label": label})
	logger.Info("Removing label on issue")
	client := newGithubClient(s.Config.GithubAccessToken)
	_, err := client.Issues.RemoveLabelForIssue(context.Background(), repoOwner, repoName, number, label)
	if err != nil {
		logger.WithError(err).Error("Error removing the label")
	}
}

func (s *Server) addLabel(repoOwner, repoName string, number int, label string) {
	logger := s.Logger.WithFields(logrus.Fields{"issue": number, "label": label})
	logger.Info("Adding label on issue")
	client := newGithubClient(s.Config.GithubAccessToken)
	_, _, err := client.Issues.AddLabelsToIssue(context.Background(), repoOwner, repoName, number, []string{label})
	if err != nil {
		logger.WithError(err).Error("Error adding the label")
	}
}

func (s *Server) getComments(repoOwner, repoName string, number int) ([]*github.IssueComment, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	comments, _, err := client.Issues.ListComments(context.Background(), repoOwner, repoName, number, nil)
	if err != nil {
		return nil, err
	}
	return comments, nil
}

// GetUpdateChecks retrieve updated status checks from GH
func (s *Server) GetUpdateChecks(owner, repoName string, prNumber int) (*model.PullRequest, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	prGitHub, _, err := client.PullRequests.Get(context.Background(), owner, repoName, prNumber)
	pr, err := s.GetPullRequestFromGithub(prGitHub)
	if err != nil {
		return nil, err
	}

	return pr, nil
}

func (s *Server) checkUserPermission(user, repoOwner string) bool {
	client := newGithubClient(s.Config.GithubAccessToken)

	_, resp, err := client.Organizations.GetOrgMembership(context.Background(), user, repoOwner)
	if resp.StatusCode == 404 {
		s.Logger.Warnf("User %s is not part of the ORG %s", user, repoOwner)
		return false
	}
	if err != nil {
		s.Logger.WithError(err).Error("failed to get org membership")
		return false
	}

	return true
}

func (s *Server) checkIfRefExists(pr *model.PullRequest, org string, ref string) (bool, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	_, response, err := client.Git.GetRef(context.Background(), org, pr.RepoName, ref)
	if err != nil {
		return false, err
	}

	logger := s.Logger.WithFields(logrus.Fields{"ref": ref, "pr": pr.Number})
	switch response.StatusCode {
	case 200:
		logger.Debug("Reference found")
		return true, nil
	case 404:
		logger.Debug("Unable to find reference")
		return false, nil
	default:
		logger.Warnf("Unknown response %d code while trying to check for reference.", response.StatusCode)
		return false, nil
	}
}

func (s *Server) createRef(pr *model.PullRequest, ref string) {
	client := newGithubClient(s.Config.GithubAccessToken)
	_, _, err := client.Git.CreateRef(
		context.Background(),
		pr.RepoOwner,
		pr.RepoName,
		&github.Reference{
			Ref: github.String(ref),
			Object: &github.GitObject{
				SHA: github.String(pr.Sha),
			},
		})
	if err != nil {
		s.Logger.WithError(err).Error("Failed to create reference")
	}
}

func (s *Server) deleteRefWhereCombinedStateEqualsSuccess(repoOwner string, repoName string, ref string) error {
	client := newGithubClient(s.Config.GithubAccessToken)
	cStatus, _, _ := client.Repositories.GetCombinedStatus(context.Background(), repoOwner, repoName, ref, nil)
	if cStatus.GetState() == "success" {
		_, err := client.Git.DeleteRef(context.Background(), repoOwner, repoName, ref)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) deleteRef(repoOwner string, repoName string, ref string) error {
	client := newGithubClient(s.Config.GithubAccessToken)
	_, err := client.Git.DeleteRef(context.Background(), repoOwner, repoName, ref)
	if err != nil {
		return err
	}
	return nil
}

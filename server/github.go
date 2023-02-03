// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"io"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/matterwick/model"

	"github.com/google/go-github/v32/github"
	"golang.org/x/oauth2"
)

// PullRequestEventFromJSON parses json to a github.PullRequestEvent
func PullRequestEventFromJSON(data io.Reader) *github.PullRequestEvent {
	decoder := json.NewDecoder(data)
	var event github.PullRequestEvent
	if err := decoder.Decode(&event); err != nil {
		mlog.Error("error parsing pull request event from JSON", mlog.Err(err))
		return nil
	}

	return &event
}

// IssueCommentEventFromJSON parses json to a github.IssueCommentEvent
func IssueCommentEventFromJSON(data io.Reader) *github.IssueCommentEvent {
	decoder := json.NewDecoder(data)
	var event github.IssueCommentEvent
	if err := decoder.Decode(&event); err != nil {
		mlog.Error("error parsing issue comment from JSON", mlog.Err(err))
		return nil
	}

	return &event
}

// PingEventFromJSON parses json to a github.PingEvent
func PingEventFromJSON(data io.Reader) *github.PingEvent {
	decoder := json.NewDecoder(data)
	var event github.PingEvent
	if err := decoder.Decode(&event); err != nil {
		mlog.Error("error parsing ping event from JSON", mlog.Err(err))
		return nil
	}

	return &event
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
		HeadLabel: *pullRequest.Head.Label,
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

func labelsToStringArray(labels []*github.Label) []string {
	out := make([]string, len(labels))

	for i, label := range labels {
		out[i] = *label.Name
	}

	return out
}

func (s *Server) sendGitHubComment(repoOwner, repoName string, number int, comment string) {
	mlog.Debug("Sending GitHub comment", mlog.Int("issue", number), mlog.String("comment", comment))
	client := newGithubClient(s.Config.GithubAccessToken)
	_, _, err := client.Issues.CreateComment(context.Background(), repoOwner, repoName, number, &github.IssueComment{Body: &comment})
	if err != nil {
		mlog.Error("Error commenting", mlog.Err(err))
	}
}

func (s *Server) removeLabel(repoOwner, repoName string, number int, label string) {
	mlog.Info("Removing label on issue", mlog.Int("issue", number), mlog.String("label", label))
	client := newGithubClient(s.Config.GithubAccessToken)
	_, err := client.Issues.RemoveLabelForIssue(context.Background(), repoOwner, repoName, number, label)
	if err != nil {
		mlog.Error("Error removing the label", mlog.Err(err))
	}
	mlog.Info("Finished removing the label")
}

func (s *Server) getComments(repoOwner, repoName string, number int) ([]*github.IssueComment, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	comments, _, err := client.Issues.ListComments(context.Background(), repoOwner, repoName, number, nil)
	if err != nil {
		mlog.Error("pr_error", mlog.Err(err))
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
		mlog.Error("pr_error", mlog.Err(err))
		return nil, err
	}

	return pr, nil
}

func (s *Server) checkUserPermission(user, repoOwner string) bool {
	client := newGithubClient(s.Config.GithubAccessToken)

	_, resp, err := client.Organizations.GetOrgMembership(context.Background(), user, repoOwner)
	if resp.StatusCode == 404 {
		mlog.Info("User is not part of the ORG", mlog.String("User", user))
		return false
	}
	if err != nil {
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

	if response.StatusCode == 200 {
		mlog.Debug("Reference found. ", mlog.Int("pr", pr.Number), mlog.String("ref", ref))
		return true, nil
	} else if response.StatusCode == 404 {
		mlog.Debug("Unable to find reference. ", mlog.Int("pr", pr.Number), mlog.String("ref", ref))
		return false, nil
	} else {
		mlog.Debug("Unknown response code while trying to check for reference. ", mlog.Int("pr", pr.Number), mlog.Int("response_code", response.StatusCode), mlog.String("ref", ref))
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
		mlog.Error("Error creating reference", mlog.Err(err))
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

func (s *Server) areChecksSuccessfulForPr(pr *model.PullRequest, org string) (bool, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	mlog.Debug("Checking combined status for ref", mlog.Int("prNumber", pr.Number), mlog.String("ref", pr.Ref), mlog.String("prSha", pr.Sha))
	cStatus, _, err := client.Repositories.GetCombinedStatus(context.Background(), org, pr.RepoName, pr.Sha, nil)
	if err != nil {
		mlog.Err(err)
		return false, err
	}
	mlog.Debug("Retrieved status for pr", mlog.String("status", cStatus.GetState()), mlog.Int("prNumber", pr.Number), mlog.String("prSha", pr.Sha))
	if cStatus.GetState() == "success" || cStatus.GetState() == "" {
		return true, nil
	}
	return false, nil
}

func (s *Server) pullRequestWithBranchNameExists(pr *model.PullRequest) (*model.PullRequest, error) {
	client := newGithubClient(s.Config.GithubAccessToken)
	prs, _, err := client.PullRequests.List(context.Background(), pr.RepoOwner, pr.RepoName, &github.PullRequestListOptions{
		Head:  pr.HeadLabel,
		State: "open",
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	})
	if err != nil {
		return nil, err
	}

	if len(prs) == 0 {
		return nil, nil
	}

	return s.GetPullRequestFromGithub(prs[0])
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v32/github"
	"github.com/mattermost/matterwick/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthorizeMobileE2ELabel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		headRepo   string
		sender     string
		permission string
		status     int
		wantLookup bool
		wantError  bool
	}{
		{name: "writer", headRepo: "contributor/mattermost-mobile", sender: "maintainer", permission: "write", wantLookup: true},
		{name: "administrator", headRepo: "contributor/mattermost-mobile", sender: "maintainer", permission: "admin", wantLookup: true},
		{name: "maintainer", headRepo: "contributor/mattermost-mobile", sender: "maintainer", permission: "maintain", wantLookup: true},
		{name: "triage", headRepo: "contributor/mattermost-mobile", sender: "triager", permission: "triage", wantLookup: true, wantError: true},
		{name: "read", headRepo: "contributor/mattermost-mobile", sender: "reader", permission: "read", wantLookup: true, wantError: true},
		{name: "unknown permission", headRepo: "contributor/mattermost-mobile", sender: "reader", wantLookup: true, wantError: true},
		{name: "lookup failure", headRepo: "contributor/mattermost-mobile", sender: "maintainer", status: http.StatusForbidden, wantLookup: true, wantError: true},
		{name: "missing sender", headRepo: "contributor/mattermost-mobile", wantError: true},
		{name: "missing head repository", sender: "maintainer", wantError: true},
		{name: "same repository bot", headRepo: "Mattermost/Mattermost-Mobile", sender: "bot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/repos/mattermost/mattermost-mobile/collaborators/"+tc.sender+"/permission", r.URL.Path)
				lookups++
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"permission": tc.permission})
			}))
			t.Cleanup(gh.Close)
			s := newDryRunServer(t, "", "mattermost")
			s.githubAPIBase = gh.URL + "/"
			base := &github.Repository{
				FullName: github.String("mattermost/mattermost-mobile"),
				Name:     github.String("mattermost-mobile"),
				Owner:    &github.User{Login: github.String("mattermost")},
			}
			event := &github.PullRequestEvent{
				Action: github.String("labeled"),
				Repo:   base,
				Label:  &github.Label{Name: github.String("E2E/Run")},
				Sender: &github.User{Login: github.String(tc.sender)},
				PullRequest: &github.PullRequest{
					Head: &github.PullRequestBranch{Repo: &github.Repository{FullName: github.String(tc.headRepo)}},
					Base: &github.PullRequestBranch{Repo: base},
				},
			}
			if tc.wantError {
				// Rejected events must return before attempting PR processing or provisioning.
				require.NotPanics(t, func() { s.handlePullRequestEvent(event) })
				require.Error(t, s.authorizeMobileE2ELabel(event))
			} else {
				require.NoError(t, s.authorizeMobileE2ELabel(event))
			}
			wantLookups := 0
			if tc.wantLookup {
				wantLookups = 1
				if tc.wantError {
					wantLookups = 2
				}
			}
			assert.Equal(t, wantLookups, lookups)
		})
	}
}

func TestMobilePRDispatchWorkflowRef(t *testing.T) {
	for _, tc := range []struct {
		name          string
		headRepo      string
		defaultBranch string
		repoStatus    int
		wantRef       string
		wantLookup    bool
		wantError     bool
	}{
		{name: "fork", headRepo: "contributor/mattermost-mobile", defaultBranch: "main", wantRef: "main", wantLookup: true},
		{name: "configured default branch", headRepo: "contributor/mattermost-mobile", defaultBranch: "release-default", wantRef: "release-default", wantLookup: true},
		{name: "same repository", headRepo: "Mattermost/Mattermost-Mobile", wantRef: "feature"},
		{name: "missing head repository", wantError: true},
		{name: "missing default branch", headRepo: "contributor/mattermost-mobile", wantLookup: true, wantError: true},
		{name: "repository lookup fails", headRepo: "contributor/mattermost-mobile", repoStatus: http.StatusForbidden, wantLookup: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captures []dispatchCapture
			lookups := 0
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/mattermost-mobile":
					lookups++
					if tc.repoStatus != 0 {
						w.WriteHeader(tc.repoStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": tc.defaultBranch})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/mattermost/mattermost-mobile/actions/workflows/e2e-detox-pr.yml/dispatches":
					var capture dispatchCapture
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&capture))
					captures = append(captures, capture)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(gh.Close)
			s := newDryRunServer(t, "", "mattermost")
			pr := &model.PullRequest{
				RepoOwner: "mattermost",
				RepoName:  "mattermost-mobile",
				FullName:  tc.headRepo,
				Number:    42,
				Ref:       "feature",
				Sha:       strings.Repeat("a", 40),
			}
			err := s.triggerMobileE2EWorkflow(context.Background(), newTestGitHubClient(t, gh), pr, makeMobileInstances(), "ios")
			assert.Equal(t, tc.wantLookup, lookups == 1)
			assert.Equal(t, "feature", pr.Ref, "dispatch selection must preserve the PR branch for display")
			if tc.wantError {
				require.Error(t, err)
				require.Empty(t, captures, "failed fork classification must never dispatch a branch name")
				return
			}
			require.NoError(t, err)
			require.Len(t, captures, 1)
			assert.Equal(t, tc.wantRef, captures[0].Ref)
			assert.Equal(t, pr.Sha, captures[0].Inputs["MOBILE_VERSION"])
			assert.Equal(t, "42", captures[0].Inputs["pr_number"])
			assert.Equal(t, "ios", captures[0].Inputs["PLATFORM"])
		})
	}
}

func TestMobilePRProvisionAndReuseKeepApprovedSHA(t *testing.T) {
	originalCreate := e2eCreatePRInstances
	t.Cleanup(func() { e2eCreatePRInstances = originalCreate })
	e2eCreatePRInstances = func(_ *Server, _ *model.PullRequest, _ string, _ []string) ([]*E2EInstance, error) {
		return makeMobileInstances(), nil
	}
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			approvedSHA := strings.Repeat("a", 40)
			newHeadSHA := strings.Repeat("b", 40)
			var captures []dispatchCapture
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/mattermost-mobile":
					_, _ = w.Write([]byte(`{"default_branch":"main"}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"state": "open", "head": map[string]string{"sha": newHeadSHA},
						"labels": []map[string]string{{"name": "E2E/Run-iOS"}},
					})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runs"):
					_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
					var capture dispatchCapture
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&capture))
					captures = append(captures, capture)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(gh.Close)
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				if r.URL.Path == "/api/installations" {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				_, _ = w.Write([]byte(`{"ID":"inst","State":"stable"}`))
			}))
			t.Cleanup(cloud.Close)
			s := newDryRunServer(t, "", "mattermost")
			s.githubAPIBase = gh.URL + "/"
			s.CloudClient = model.NewCloudClient(cloud.URL, "", "", "", "")
			s.e2eInProgress = make(map[string]bool)
			pr := &model.PullRequest{
				RepoOwner: "mattermost", RepoName: "mattermost-mobile", FullName: "contributor/mattermost-mobile",
				Number: 42, Ref: "feature", Sha: approvedSHA,
			}
			if reuse {
				s.e2eInstances["mattermost-mobile-pr-42"] = makeMobileInstances()
			}
			s.handleE2ETestRequest(pr, "E2E/Run-iOS")
			require.Len(t, captures, 1)
			assert.Equal(t, "main", captures[0].Ref)
			assert.Equal(t, approvedSHA, captures[0].Inputs["MOBILE_VERSION"], "an inherited label must not authorize the new PR head")
			assert.Equal(t, "42", captures[0].Inputs["pr_number"])
		})
	}
}

func TestCancelMobileRunsUsesPRIdentity(t *testing.T) {
	sha := strings.Repeat("a", 40)
	runs := []map[string]interface{}{
		{"id": 1, "head_branch": "main", "display_title": "E2E PR #42 @ " + sha, "status": "in_progress"},
		{"id": 2, "head_branch": "main", "display_title": "E2E PR #43 @ " + sha, "status": "in_progress"},
		{"id": 3, "head_branch": "feature", "display_title": "E2E Detox", "status": "in_progress"},
		{"id": 4, "head_branch": "feature", "display_title": "E2E PR #42 @ " + strings.Repeat("b", 40), "status": "in_progress"},
		{"id": 5, "head_branch": "feature", "display_title": "E2E PR #42 @ " + sha + " extra", "status": "in_progress"},
		{"id": 6, "head_branch": "main", "display_title": "E2E PR #42 @ abc", "status": "in_progress"},
		{"id": 7, "head_branch": "feature", "display_title": "E2E PR #42 @ " + sha, "status": "completed"},
		{"id": 8, "head_branch": "feature", "display_title": "E2E PR #42 @ " + strings.Repeat("A", 40), "status": "in_progress"},
	}
	var cancelled, inspected []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workflows/e2e-detox-pr.yml/runs"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"workflow_runs": runs})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
			inspected = append(inspected, r.URL.Path)
			_, _ = w.Write([]byte(`{"jobs":[{"name":"build-ios-simulator","status":"in_progress"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			cancelled = append(cancelled, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	s := newDryRunServer(t, "", "mattermost")
	s.githubAPIBase = gh.URL + "/"
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile", Number: 42, Ref: "feature", Sha: sha}
	s.cancelPRWorkflowRuns(pr, s.Logger, "ios")
	var wantCancelled, wantInspected []string
	for _, id := range []int{1, 4} {
		wantCancelled = append(wantCancelled, fmt.Sprintf("/repos/mattermost/mattermost-mobile/actions/runs/%d/cancel", id))
		wantInspected = append(wantInspected, fmt.Sprintf("/repos/mattermost/mattermost-mobile/actions/runs/%d/jobs", id))
	}
	assert.Equal(t, wantCancelled, cancelled, "only this PR's identified runs may be cancelled, including older approved revisions")
	assert.Equal(t, wantInspected, inspected, "unidentified or other PR runs must not be inspected for cancellation")
}

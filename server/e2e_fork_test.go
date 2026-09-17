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

// Both apps share the same trust boundary but use different workflow inputs.
var forkE2EApps = []struct {
	repo, workflow, defaultBranch, versionInput, label, platform string
	instances                                                    func() []*E2EInstance
}{
	{"mattermost-mobile", "e2e-detox-pr.yml", "main", "MOBILE_VERSION", "E2E/Run-iOS", "ios", makeMobileInstances},
	{"desktop", "e2e-functional.yml", "master", "version_name", "E2E/Run", "all", makeDesktopInstances},
}

func TestAuthorizeE2ELabel(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				headRepo   string
				sender     string
				permission string
				status     int
				wantLookup bool
				wantError  bool
			}{
				{name: "writer", headRepo: "contributor/" + app.repo, sender: "maintainer", permission: "write", wantLookup: true},
				{name: "administrator", headRepo: "contributor/" + app.repo, sender: "maintainer", permission: "admin", wantLookup: true},
				{name: "maintainer", headRepo: "contributor/" + app.repo, sender: "maintainer", permission: "maintain", wantLookup: true},
				{name: "triage", headRepo: "contributor/" + app.repo, sender: "triager", permission: "triage", wantLookup: true, wantError: true},
				{name: "read", headRepo: "contributor/" + app.repo, sender: "reader", permission: "read", wantLookup: true, wantError: true},
				{name: "unknown permission", headRepo: "contributor/" + app.repo, sender: "reader", wantLookup: true, wantError: true},
				{name: "lookup failure", headRepo: "contributor/" + app.repo, sender: "maintainer", status: http.StatusForbidden, wantLookup: true, wantError: true},
				{name: "missing sender", headRepo: "contributor/" + app.repo, wantError: true},
				{name: "missing head repository", sender: "maintainer", wantError: true},
				{name: "same repository bot", headRepo: strings.ToUpper("mattermost/" + app.repo), sender: "bot"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					lookups := 0
					gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						assert.Equal(t, http.MethodGet, r.Method)
						assert.Equal(t, "/repos/mattermost/"+app.repo+"/collaborators/"+tc.sender+"/permission", r.URL.Path)
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
						FullName: github.String("mattermost/" + app.repo),
						Name:     github.String(app.repo),
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
						require.Error(t, s.authorizeE2ELabel(event))
					} else {
						require.NoError(t, s.authorizeE2ELabel(event))
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
		})
	}
}

func TestPRDispatchWorkflowRef(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				headRepo      string
				defaultBranch string
				repoStatus    int
				wantRef       string
				wantLookup    bool
				missingSHA    bool
				wantError     bool
			}{
				{name: "fork", headRepo: "contributor/" + app.repo, defaultBranch: app.defaultBranch, wantRef: app.defaultBranch, wantLookup: true},
				{name: "configured default branch", headRepo: "contributor/" + app.repo, defaultBranch: "release-default", wantRef: "release-default", wantLookup: true},
				{name: "same repository", headRepo: strings.ToUpper("mattermost/" + app.repo), wantRef: "feature"},
				{name: "missing head repository", wantError: true},
				{name: "missing approved SHA", headRepo: "contributor/" + app.repo, missingSHA: true, wantError: true},
				{name: "missing default branch", headRepo: "contributor/" + app.repo, wantLookup: true, wantError: true},
				{name: "repository lookup fails", headRepo: "contributor/" + app.repo, repoStatus: http.StatusForbidden, wantLookup: true, wantError: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var captures []dispatchCapture
					lookups := 0
					gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/"+app.repo:
							lookups++
							if tc.repoStatus != 0 {
								w.WriteHeader(tc.repoStatus)
								return
							}
							_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": tc.defaultBranch})
						case r.Method == http.MethodPost && r.URL.Path == "/repos/mattermost/"+app.repo+"/actions/workflows/"+app.workflow+"/dispatches":
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
						RepoName:  app.repo,
						FullName:  tc.headRepo,
						Number:    42,
						Ref:       "feature",
						Sha:       strings.Repeat("a", 40),
					}
					if tc.missingSHA {
						pr.Sha = ""
					}
					client := newTestGitHubClient(t, gh)
					var err error
					if app.repo == "desktop" {
						err = s.triggerDesktopE2EWorkflow(context.Background(), client, pr, app.instances())
					} else {
						err = s.triggerMobileE2EWorkflow(context.Background(), client, pr, app.instances(), app.platform)
					}
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
					assert.Equal(t, pr.Sha, captures[0].Inputs[app.versionInput])
					assert.Equal(t, "42", captures[0].Inputs["pr_number"])
					if app.repo == "desktop" {
						assert.NotContains(t, captures[0].Inputs, "PLATFORM")
					} else {
						assert.Equal(t, app.platform, captures[0].Inputs["PLATFORM"])
					}
				})
			}
		})
	}
}

func TestPRProvisionAndReuseKeepApprovedSHA(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			originalCreate := e2eCreatePRInstances
			t.Cleanup(func() { e2eCreatePRInstances = originalCreate })
			e2eCreatePRInstances = func(_ *Server, _ *model.PullRequest, _ string, _ []string) ([]*E2EInstance, error) {
				return app.instances(), nil
			}
			for _, reuse := range []bool{false, true} {
				t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
					approvedSHA := strings.Repeat("a", 40)
					newHeadSHA := strings.Repeat("b", 40)
					var captures []dispatchCapture
					gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/"+app.repo:
							_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": app.defaultBranch})
						case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
							_ = json.NewEncoder(w).Encode(map[string]interface{}{
								"state": "open", "head": map[string]string{"sha": newHeadSHA},
								"labels": []map[string]string{{"name": app.label}},
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
						RepoOwner: "mattermost", RepoName: app.repo, FullName: "contributor/" + app.repo,
						Number: 42, Ref: "feature", Sha: approvedSHA,
					}
					if reuse {
						s.e2eInstances[app.repo+"-pr-42"] = app.instances()
					}
					s.handleE2ETestRequest(pr, app.label)
					require.Len(t, captures, 1)
					assert.Equal(t, app.defaultBranch, captures[0].Ref)
					assert.Equal(t, approvedSHA, captures[0].Inputs[app.versionInput], "an inherited label must not authorize the new PR head")
					assert.Equal(t, "42", captures[0].Inputs["pr_number"])
				})
			}
		})
	}
}

func TestCancelRunsUsesPRIdentity(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			runs := []map[string]interface{}{
				{"id": 1, "head_branch": app.defaultBranch, "display_title": "E2E PR #42 @ " + sha, "status": "in_progress"},
				{"id": 2, "head_branch": app.defaultBranch, "display_title": "E2E PR #43 @ " + sha, "status": "in_progress"},
				{"id": 3, "head_branch": "feature", "display_title": "E2E Detox", "status": "in_progress"},
				{"id": 4, "head_branch": "feature", "display_title": "E2E PR #42 @ " + strings.Repeat("b", 40), "status": "in_progress"},
				{"id": 5, "head_branch": "feature", "display_title": "E2E PR #42 @ " + sha + " extra", "status": "in_progress"},
				{"id": 6, "head_branch": app.defaultBranch, "display_title": "E2E PR #42 @ abc", "status": "in_progress"},
				{"id": 7, "head_branch": "feature", "display_title": "E2E PR #42 @ " + sha, "status": "completed"},
				{"id": 8, "head_branch": "feature", "display_title": "E2E PR #42 @ " + strings.Repeat("A", 40), "status": "in_progress"},
			}
			var cancelled, inspected []string
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workflows/"+app.workflow+"/runs"):
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
			pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: app.repo, Number: 42, Ref: "feature", Sha: sha}
			s.cancelPRWorkflowRuns(pr, s.Logger, app.platform)
			var wantCancelled, wantInspected []string
			for _, id := range []int{1, 4} {
				wantCancelled = append(wantCancelled, fmt.Sprintf("/repos/mattermost/%s/actions/runs/%d/cancel", app.repo, id))
				if app.repo != "desktop" {
					wantInspected = append(wantInspected, fmt.Sprintf("/repos/mattermost/%s/actions/runs/%d/jobs", app.repo, id))
				}
			}
			assert.Equal(t, wantCancelled, cancelled, "only this PR's identified runs may be cancelled, including older approved revisions")
			assert.Equal(t, wantInspected, inspected, "unidentified or other PR runs must not be inspected for cancellation")
		})
	}
}

func TestPRCompletionDoesNotCleanUpDefaultBranchServers(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			for _, prRun := range []bool{true, false} {
				t.Run(fmt.Sprintf("pr=%t", prRun), func(t *testing.T) {
					s := newDryRunServer(t, "", "mattermost")
					s.Config.E2ETestWorkflowNames = []string{"E2E"}
					pushKey := app.repo + "-push-" + app.defaultBranch + "-" + sha
					prKey := app.repo + "-pr-42"
					s.e2eInstances[pushKey] = nil
					s.e2eInstances[prKey] = nil
					title := "E2E MASTER version=" + sha
					if prRun {
						title = "E2E PR #42 @ " + strings.Repeat("b", 40)
					}
					payload, err := ParseWorkflowRunEventWithInputs(strings.NewReader(fmt.Sprintf(`{
						"action":"completed",
						"workflow_run":{"id":123,"name":"E2E","event":"workflow_dispatch","head_branch":%q,"head_sha":%q,"display_title":%q},
						"repository":{"name":%q,"owner":{"login":"mattermost"}}
					}`, app.defaultBranch, sha, title, app.repo)))
					require.NoError(t, err)
					s.handleWorkflowRunEventWithInputs(payload)
					_, pushRetained := s.e2eInstances[pushKey]
					assert.Equal(t, prRun, pushRetained, "a PR workflow's source SHA must not clean up main/push servers")
					assert.Contains(t, s.e2eInstances, prKey, "PR servers are retained until PR cleanup")
				})
			}
		})
	}
}

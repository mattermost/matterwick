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
						if strings.HasSuffix(r.URL.Path, "/pulls/42") {
							_ = json.NewEncoder(w).Encode(map[string]interface{}{
								"state":  "open",
								"labels": []map[string]string{{"name": "E2E/Run"}},
							})
							return
						}
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
						Number: github.Int(42),
						Repo:   base,
						Label:  &github.Label{Name: github.String("E2E/Run")},
						Sender: &github.User{Login: github.String(tc.sender)},
						PullRequest: &github.PullRequest{
							Number: github.Int(42),
							Head:   &github.PullRequestBranch{Repo: &github.Repository{FullName: github.String(tc.headRepo)}},
							Base:   &github.PullRequestBranch{Repo: base},
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
					s.Config.E2ETrustedForkDispatch = true
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
					s.Config.E2ETrustedForkDispatch = true
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
			s.e2eDefaultBranch = app.defaultBranch
			pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: app.repo, Number: 42, Ref: "feature", Sha: sha}
			s.cancelPRWorkflowRuns(pr, s.Logger, app.platform)
			var wantCancelled, wantInspected []string
			for _, id := range []int{1, 3, 4, 5, 8} {
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

var productionE2EWorkflows = []struct {
	repo, yamlName, defaultBranch string
}{
	{"desktop", "Electron Playwright Tests", "master"},
	{"mattermost-mobile", "E2E", "main"},
}

func completionPayload(t *testing.T, repo, yamlName, runName, displayTitle, headBranch, headSHA string) *WorkflowRunWebhookPayload {
	t.Helper()
	payload, err := ParseWorkflowRunEventWithInputs(strings.NewReader(fmt.Sprintf(`{
		"action":"completed",
		"workflow":{"name":%q},
		"workflow_run":{"id":123,"name":%q,"event":"workflow_dispatch","head_branch":%q,"head_sha":%q,"display_title":%q},
		"repository":{"name":%q,"default_branch":%q,"owner":{"login":"mattermost"}}
	}`, yamlName, runName, headBranch, headSHA, displayTitle, repo, headBranch)))
	require.NoError(t, err)
	return payload
}

func TestUnidentifiedDefaultBranchCompletionRetainsPushServers(t *testing.T) {
	pushSHA := strings.Repeat("a", 40)
	for _, app := range productionE2EWorkflows {
		for _, title := range []string{app.yamlName, "E2E"} {
			t.Run(app.repo+"/"+title, func(t *testing.T) {
				s := newDryRunServer(t, "", "mattermost")
				s.Config.E2ETestWorkflowNames = []string{"Electron Playwright Tests", "E2E", "Compatibility Matrix Testing"}
				s.e2eDefaultBranch = app.defaultBranch
				pushKey := app.repo + "-push-" + app.defaultBranch + "-" + pushSHA
				prKey := app.repo + "-pr-42"
				s.e2eInstances[pushKey] = nil
				s.e2eInstances[prKey] = nil
				s.handleWorkflowRunEventWithInputs(completionPayload(t, app.repo, app.yamlName, title, title, app.defaultBranch, pushSHA))
				assert.Contains(t, s.e2eInstances, pushKey, "unidentified default-branch dispatch must not SHA-destroy push servers")
				assert.Contains(t, s.e2eInstances, prKey)
			})
		}
	}
}

func TestUnidentifiedDefaultBranchUnknownLookupRetainsPushServers(t *testing.T) {
	pushSHA := strings.Repeat("a", 40)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/desktop" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(gh.Close)

	s := newDryRunServer(t, "", "mattermost")
	s.Config.E2ETestWorkflowNames = []string{"Electron Playwright Tests", "E2E", "Compatibility Matrix Testing"}
	s.githubAPIBase = gh.URL + "/"
	require.Empty(t, s.e2eDefaultBranch)

	pushKey := "desktop-push-master-" + pushSHA
	s.e2eInstances[pushKey] = nil

	payload, err := ParseWorkflowRunEventWithInputs(strings.NewReader(fmt.Sprintf(`{
		"action":"completed",
		"workflow":{"name":"Electron Playwright Tests"},
		"workflow_run":{"id":123,"name":"E2E","event":"workflow_dispatch","head_branch":"master","head_sha":%q,"display_title":"E2E"},
		"repository":{"name":"desktop","owner":{"login":"mattermost"}}
	}`, pushSHA)))
	require.NoError(t, err)
	_, hasDefault := payload.Repository["default_branch"]
	require.False(t, hasDefault, "payload must omit default_branch so lookup is the only source")

	s.handleWorkflowRunEventWithInputs(payload)
	assert.Contains(t, s.e2eInstances, pushKey, "unknown default-branch lookup must not SHA-destroy push servers")
}

func TestIdentifiedMasterCompletionDestroysPushServers(t *testing.T) {
	pushSHA := strings.Repeat("a", 40)
	for _, app := range productionE2EWorkflows {
		t.Run(app.repo, func(t *testing.T) {
			s := newDryRunServer(t, "", "mattermost")
			s.Config.E2ETestWorkflowNames = []string{"Electron Playwright Tests", "E2E", "Compatibility Matrix Testing"}
			s.e2eDefaultBranch = app.defaultBranch
			pushKey := app.repo + "-push-" + app.defaultBranch + "-" + pushSHA
			prKey := app.repo + "-pr-42"
			s.e2eInstances[pushKey] = nil
			s.e2eInstances[prKey] = nil
			title := "E2E MASTER @ " + pushSHA
			s.handleWorkflowRunEventWithInputs(completionPayload(t, app.repo, app.yamlName, title, title, app.defaultBranch, pushSHA))
			assert.NotContains(t, s.e2eInstances, pushKey, "identified MASTER completion still SHA-destroys")
			assert.Contains(t, s.e2eInstances, prKey)
		})
	}
}

func TestIdentifiedPRCompletionRetainsPushAndPRServers(t *testing.T) {
	pushSHA := strings.Repeat("a", 40)
	prSHA := strings.Repeat("b", 40)
	for _, app := range productionE2EWorkflows {
		t.Run(app.repo, func(t *testing.T) {
			s := newDryRunServer(t, "", "mattermost")
			s.Config.E2ETestWorkflowNames = []string{"Electron Playwright Tests", "E2E", "Compatibility Matrix Testing"}
			s.e2eDefaultBranch = app.defaultBranch
			pushKey := app.repo + "-push-" + app.defaultBranch + "-" + pushSHA
			prKey := app.repo + "-pr-42"
			s.e2eInstances[pushKey] = nil
			s.e2eInstances[prKey] = nil
			title := "E2E PR #42 @ " + prSHA
			s.handleWorkflowRunEventWithInputs(completionPayload(t, app.repo, app.yamlName, app.yamlName, title, app.defaultBranch, pushSHA))
			assert.Contains(t, s.e2eInstances, pushKey)
			assert.Contains(t, s.e2eInstances, prKey)
		})
	}
}

func TestRunNameIdentityUsesWorkflowYAMLName(t *testing.T) {
	pushSHA := strings.Repeat("a", 40)
	prSHA := strings.Repeat("b", 40)
	for _, app := range productionE2EWorkflows {
		t.Run(app.repo, func(t *testing.T) {
			s := newDryRunServer(t, "", "mattermost")
			s.Config.E2ETestWorkflowNames = []string{"Electron Playwright Tests", "E2E", "Compatibility Matrix Testing"}
			s.e2eDefaultBranch = app.defaultBranch
			pushKey := app.repo + "-push-" + app.defaultBranch + "-" + pushSHA
			prKey := app.repo + "-pr-42"
			s.e2eInstances[pushKey] = nil
			s.e2eInstances[prKey] = nil
			title := "E2E PR #42 @ " + prSHA
			// GitHub may copy run-name onto workflow_run.name while workflow.name stays the YAML name.
			s.handleWorkflowRunEventWithInputs(completionPayload(t, app.repo, app.yamlName, title, title, app.defaultBranch, pushSHA))
			assert.Contains(t, s.e2eInstances, pushKey)
			assert.Contains(t, s.e2eInstances, prKey)
		})
	}
}

func TestTrustedForkDispatchDisabledByDefault(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			var captures []dispatchCapture
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/"+app.repo:
					_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": app.defaultBranch})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
					var capture dispatchCapture
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&capture))
					captures = append(captures, capture)
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(gh.Close)
			s := newDryRunServer(t, "", "mattermost")
			require.False(t, s.Config.E2ETrustedForkDispatch)
			pr := &model.PullRequest{
				RepoOwner: "mattermost", RepoName: app.repo, FullName: "contributor/" + app.repo,
				Number: 42, Ref: "feature", Sha: strings.Repeat("a", 40),
			}
			client := newTestGitHubClient(t, gh)
			var err error
			if app.repo == "desktop" {
				err = s.triggerDesktopE2EWorkflow(context.Background(), client, pr, app.instances())
			} else {
				err = s.triggerMobileE2EWorkflow(context.Background(), client, pr, app.instances(), app.platform)
			}
			require.Error(t, err)
			assert.Empty(t, captures)
		})
	}
}

func TestTrustedForkDispatchEnabledUsesDefaultBranch(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			var captures []dispatchCapture
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/mattermost/"+app.repo:
					_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": app.defaultBranch})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
					var capture dispatchCapture
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&capture))
					captures = append(captures, capture)
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(gh.Close)
			s := newDryRunServer(t, "", "mattermost")
			s.Config.E2ETrustedForkDispatch = true
			pr := &model.PullRequest{
				RepoOwner: "mattermost", RepoName: app.repo, FullName: "contributor/" + app.repo,
				Number: 42, Ref: "feature", Sha: strings.Repeat("a", 40),
			}
			client := newTestGitHubClient(t, gh)
			var err error
			if app.repo == "desktop" {
				err = s.triggerDesktopE2EWorkflow(context.Background(), client, pr, app.instances())
			} else {
				err = s.triggerMobileE2EWorkflow(context.Background(), client, pr, app.instances(), app.platform)
			}
			require.NoError(t, err)
			require.Len(t, captures, 1)
			assert.Equal(t, app.defaultBranch, captures[0].Ref)
		})
	}
}

func TestStrippedE2ELabelDoesNotDispatch(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			var captures []dispatchCapture
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"state":  "open",
						"head":   map[string]string{"sha": strings.Repeat("b", 40)},
						"labels": []map[string]string{},
					})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runs"):
					_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
					var capture dispatchCapture
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&capture))
					captures = append(captures, capture)
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(gh.Close)
			s := newDryRunServer(t, "", "mattermost")
			s.Config.E2ETrustedForkDispatch = true
			s.githubAPIBase = gh.URL + "/"
			s.e2eInProgress = make(map[string]bool)
			pr := &model.PullRequest{
				RepoOwner: "mattermost", RepoName: app.repo, FullName: "contributor/" + app.repo,
				Number: 42, Ref: "feature", Sha: strings.Repeat("a", 40),
			}
			s.e2eInstances[app.repo+"-pr-42"] = app.instances()
			s.handleE2ETestRequest(pr, app.label)
			assert.Empty(t, captures, "a stripped E2E label must not dispatch the labeled SHA")
		})
	}
}

func TestCancelMatchesNameOrLegacyNonDefaultBranch(t *testing.T) {
	for _, app := range forkE2EApps {
		t.Run(app.repo, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			title := "E2E PR #42 @ " + sha
			runs := []map[string]interface{}{
				{"id": 11, "name": title, "display_title": app.defaultBranch, "head_branch": app.defaultBranch, "status": "queued"},
				{"id": 12, "name": "E2E", "display_title": "E2E", "head_branch": "feature", "status": "in_progress"},
				{"id": 13, "name": "E2E", "display_title": "E2E", "head_branch": app.defaultBranch, "status": "in_progress"},
			}
			var cancelled []string
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workflows/"+app.workflow+"/runs"):
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"workflow_runs": runs})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
					_, _ = w.Write([]byte(`{"jobs":[{"name":"build-ios-simulator","status":"in_progress"}]}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
					cancelled = append(cancelled, r.URL.Path)
					w.WriteHeader(http.StatusAccepted)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(gh.Close)
			s := newDryRunServer(t, "", "mattermost")
			s.githubAPIBase = gh.URL + "/"
			s.e2eDefaultBranch = app.defaultBranch
			pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: app.repo, Number: 42, Ref: "feature", Sha: sha}
			s.cancelPRWorkflowRuns(pr, s.Logger, app.platform)
			assert.Contains(t, strings.Join(cancelled, ","), "/actions/runs/11/cancel", "identity on name must cancel queued runs")
			assert.Contains(t, strings.Join(cancelled, ","), "/actions/runs/12/cancel", "legacy same-repo feature-branch runs remain cancellable")
			assert.NotContains(t, strings.Join(cancelled, ","), "/actions/runs/13/cancel", "unidentified default-branch runs must never be cancelled")
		})
	}
}

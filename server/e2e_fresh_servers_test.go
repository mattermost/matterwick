// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGitHub serves the workflow-runs list with runsJSON (or runsStatus if not 200) and
// accepts everything else. It records "METHOD path" and the query of each runs-list call.
type fakeGitHub struct {
	mu          sync.Mutex
	calls       []string
	listQueries []string
}

func (f *fakeGitHub) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGitHub) has(prefix string) bool {
	for _, c := range f.snapshot() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func newFakeGitHub(t *testing.T, runsStatus int, runsJSON string) (string, *fakeGitHub) {
	t.Helper()
	f := &fakeGitHub{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		isList := r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runs")
		if isList {
			f.listQueries = append(f.listQueries, r.URL.RawQuery)
		}
		f.mu.Unlock()
		if isList {
			w.WriteHeader(runsStatus)
			_, _ = w.Write([]byte(runsJSON))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(gh.Close)
	return gh.URL + "/", f
}

func TestIsPRE2EIdle(t *testing.T) {
	const key = "mattermost-mobile-pr-1"
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile", Number: 1, Ref: "feature/x"}

	for _, tt := range []struct {
		name           string
		inProgress     []string
		dispatchedAgo  time.Duration // 0 = never dispatched
		runsStatus     int
		runsJSON       string
		want           bool
		wantListCalled bool
	}{
		{
			name:           "idle: only completed runs on the branch, active run on another branch",
			inProgress:     []string{key + "-android"},
			runsStatus:     http.StatusOK,
			runsJSON:       `{"workflow_runs":[{"id":1,"head_branch":"feature/x","status":"completed"},{"id":2,"head_branch":"other","status":"in_progress"}]}`,
			want:           true,
			wantListCalled: true,
		},
		{
			name:       "another platform is provisioning",
			inProgress: []string{key + "-android", key + "-ios"},
			want:       false,
		},
		{
			name:           "a longer PR number with the same prefix does not count",
			inProgress:     []string{key + "-android", "mattermost-mobile-pr-12-ios"},
			runsStatus:     http.StatusOK,
			runsJSON:       `{"workflow_runs":[]}`,
			want:           true,
			wantListCalled: true,
		},
		{
			name:          "dispatched moments ago, before GitHub lists the run",
			inProgress:    []string{key + "-android"},
			dispatchedAgo: time.Minute,
			want:          false,
		},
		{
			name:           "dispatch older than the grace period does not count",
			inProgress:     []string{key + "-android"},
			dispatchedAgo:  e2eDispatchGracePeriod + time.Minute,
			runsStatus:     http.StatusOK,
			runsJSON:       `{"workflow_runs":[]}`,
			want:           true,
			wantListCalled: true,
		},
		{
			name:           "queued run on the branch",
			inProgress:     []string{key + "-android"},
			runsStatus:     http.StatusOK,
			runsJSON:       `{"workflow_runs":[{"id":3,"head_branch":"feature/x","status":"queued"}]}`,
			want:           false,
			wantListCalled: true,
		},
		{
			name:           "GitHub error counts as busy",
			inProgress:     []string{key + "-android"},
			runsStatus:     http.StatusInternalServerError,
			runsJSON:       `{}`,
			want:           false,
			wantListCalled: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newFakeCloudServer(t, "[]")
			base, gh := newFakeGitHub(t, tt.runsStatus, tt.runsJSON)
			s.githubAPIBase = base
			s.e2eInProgress = map[string]bool{}
			for _, k := range tt.inProgress {
				s.e2eInProgress[k] = true
			}
			if tt.dispatchedAgo > 0 {
				s.e2eLastDispatch.Store(key, time.Now().Add(-tt.dispatchedAgo))
			}

			assert.Equal(t, tt.want, s.isPRE2EIdle(pr, mobileE2EProfile.workflow, key, key+"-android", s.Logger))
			if tt.wantListCalled {
				assert.Equal(t, []string{"GET /repos/mattermost/mattermost-mobile/actions/workflows/e2e-detox-pr.yml/runs"}, gh.snapshot())
				assert.Equal(t, []string{"branch=feature%2Fx&event=workflow_dispatch&per_page=100"}, gh.listQueries)
			} else {
				assert.Empty(t, gh.snapshot())
			}
		})
	}
}

func makeE2EInstances(prefix string, platforms []string) []*E2EInstance {
	var out []*E2EInstance
	for _, p := range platforms {
		out = append(out, &E2EInstance{
			Name:           prefix + p,
			Platform:       p,
			URL:            "https://" + prefix + p + ".test.example.com",
			InstallationID: "old-" + p,
			ServerVersion:  "11.9.0",
		})
	}
	return out
}

// newE2ERequestServer returns a Server whose cloud fails every create (so a fresh run stops
// after the create attempt) and whose GitHub is faked, with existing in-memory servers.
func newE2ERequestServer(t *testing.T, repo string, existing []*E2EInstance, runsJSON string) (*Server, *fakeCloud, *fakeGitHub) {
	t.Helper()
	s, fc := newFakeCloudServer(t, "[]")
	base, gh := newFakeGitHub(t, http.StatusOK, runsJSON)
	s.githubAPIBase = base
	s.Config.E2EOxideEnabled = true
	s.e2eInProgress = map[string]bool{}
	s.e2ePRCleanupGeneration = map[string]int64{}
	s.e2eInstances[repo+"-pr-5"] = existing
	return s, fc, gh
}

func cloudCalls(fc *fakeCloud, method, pathPrefix string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	n := 0
	for _, r := range fc.requests {
		if r.Method == method && strings.HasPrefix(r.URL.Path, pathPrefix) {
			n++
		}
	}
	return n
}

func TestHandleE2ETestRequest_FreshVsReuse(t *testing.T) {
	const noRuns = `{"workflow_runs":[]}`
	const activeRun = `{"workflow_runs":[{"id":3,"head_branch":"b","status":"in_progress"}]}`

	for _, tt := range []struct {
		name      string
		repo      string
		platforms []string
		runsJSON  string
		wantFresh bool
		workflow  string
	}{
		{"mobile idle PR gets fresh servers", "mattermost-mobile", mobileE2EPlatforms, noRuns, true, "e2e-detox-pr.yml"},
		{"desktop idle PR gets fresh servers", "mattermost-desktop", []string{"linux", "macos", "windows"}, noRuns, true, "e2e-functional.yml"},
		{"mobile busy PR reuses its servers", "mattermost-mobile", mobileE2EPlatforms, activeRun, false, "e2e-detox-pr.yml"},
		{"desktop busy PR reuses its servers", "mattermost-desktop", []string{"linux", "macos", "windows"}, activeRun, false, "e2e-functional.yml"},
		{"Oxide idle PR gets fresh servers", "mattermost-mobile-oxide", oxideE2EPlatforms, noRuns, true, "e2e-matterwick.yml"},
		{"Oxide busy PR reuses its servers", "mattermost-mobile-oxide", oxideE2EPlatforms, activeRun, false, "e2e-matterwick.yml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			existing := makeE2EInstances("x-", tt.platforms)
			s, fc, gh := newE2ERequestServer(t, tt.repo, existing, tt.runsJSON)
			pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: tt.repo, FullName: "mattermost/" + tt.repo, Number: 5, Ref: "b", Sha: "s"}

			s.handleE2ETestRequest(pr, "E2E/Run")

			deletes := cloudCalls(fc, http.MethodDelete, "/api/installation/old-")
			creates := cloudCalls(fc, http.MethodPost, "/api/installations")
			dispatched := gh.has("POST /repos/mattermost/" + tt.repo + "/actions/workflows/" + tt.workflow + "/dispatches")
			if tt.wantFresh {
				assert.Equal(t, len(existing), deletes, "the PR's old servers must be torn down")
				assert.Positive(t, creates, "fresh servers must be requested")
				assert.False(t, dispatched, "the failed create must not dispatch")
				assert.True(t, gh.has("POST /repos/mattermost/"+tt.repo+"/issues/5/comments"), "a provisioning failure must be reported on the PR")
				assert.Empty(t, s.e2eInstances[tt.repo+"-pr-5"])
			} else {
				assert.Zero(t, deletes, "servers in use must not be torn down")
				assert.Zero(t, creates)
				assert.True(t, dispatched, "the run must be dispatched on the existing servers")
				assert.Equal(t, existing, s.e2eInstances[tt.repo+"-pr-5"])
			}
		})
	}
}

func TestWaitForE2EInstallationStable_FailsFast(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"update failed", http.StatusOK, `{"ID":"i","State":"update-failed"}`, `installation entered state "update-failed"`},
		{"creation failed", http.StatusOK, `{"ID":"i","State":"creation-failed"}`, `installation entered state "creation-failed"`},
		{"installation gone", http.StatusNotFound, ``, "installation i not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(ts.Close)
			s := &Server{CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := s.waitForE2EInstallationStable(ctx, "i", s.Logger)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NoError(t, ctx.Err(), "must fail on the first poll, not wait out the timeout")
		})
	}
}

func TestHandleE2ETestRequest_CloudServersThatCannotWakeAreReplaced(t *testing.T) {
	const activeRun = `{"workflow_runs":[{"id":3,"head_branch":"b","status":"in_progress"}]}`
	// Found after a matterwick restart, hibernating; the fake cloud never answers their state check.
	const list = `[{"ID":"cold-linux","OwnerID":"desktop-pr-5-linux-abcdef12","State":"hibernating"},` +
		`{"ID":"cold-macos","OwnerID":"desktop-pr-5-macos-abcdef12","State":"hibernating"},` +
		`{"ID":"cold-windows","OwnerID":"desktop-pr-5-windows-abcdef12","State":"hibernating"}]`
	stubProvisioningTimers(t)
	stubWakeTimeout(t)
	s, fc := newFakeCloudServer(t, list)
	base, gh := newFakeGitHub(t, http.StatusOK, activeRun)
	s.githubAPIBase = base
	s.e2eInProgress = map[string]bool{}
	s.e2ePRCleanupGeneration = map[string]int64{}
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-desktop", FullName: "mattermost/mattermost-desktop", Number: 5, Ref: "b", Sha: "s"}

	s.handleE2ETestRequest(pr, "E2E/Run")

	for _, id := range []string{"cold-linux", "cold-macos", "cold-windows"} {
		assert.Positive(t, cloudCalls(fc, http.MethodDelete, "/api/installation/"+id), "%s cannot be woken, so it must be torn down, not leaked", id)
	}
	assert.Positive(t, cloudCalls(fc, http.MethodPost, "/api/installations"), "fresh servers must be requested")
	assert.False(t, gh.has("POST /repos/mattermost/mattermost-desktop/actions/workflows/e2e-functional.yml/dispatches"), "no run on servers that are not serving")
}

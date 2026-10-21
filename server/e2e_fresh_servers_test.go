// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/matterwick/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeGitHub serves the workflow-runs list with runsJSON (or runsStatus if not 200) and
// accepts everything else. It returns the requested paths.
func newFakeGitHub(t *testing.T, runsStatus int, runsJSON string) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/runs") {
			w.WriteHeader(runsStatus)
			_, _ = w.Write([]byte(runsJSON))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(gh.Close)
	return gh.URL + "/", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestIsPRE2EIdle(t *testing.T) {
	const key = "mattermost-mobile-pr-1"
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile", Number: 1, Ref: "feature/x"}

	for _, tt := range []struct {
		name       string
		inProgress []string
		runsStatus int
		runsJSON   string
		want       bool
		wantGHCall bool
	}{
		{
			name:       "idle: only completed runs on the branch, active run on another branch",
			inProgress: []string{key + "-android"},
			runsStatus: http.StatusOK,
			runsJSON:   `{"workflow_runs":[{"id":1,"head_branch":"feature/x","status":"completed"},{"id":2,"head_branch":"other","status":"in_progress"}]}`,
			want:       true,
			wantGHCall: true,
		},
		{
			name:       "another platform is provisioning",
			inProgress: []string{key + "-android", key + "-ios"},
			want:       false,
		},
		{
			name:       "a longer PR number with the same prefix does not count",
			inProgress: []string{key + "-android", "mattermost-mobile-pr-12-ios"},
			runsStatus: http.StatusOK,
			runsJSON:   `{"workflow_runs":[]}`,
			want:       true,
			wantGHCall: true,
		},
		{
			name:       "queued run on the branch",
			inProgress: []string{key + "-android"},
			runsStatus: http.StatusOK,
			runsJSON:   `{"workflow_runs":[{"id":3,"head_branch":"feature/x","status":"queued"}]}`,
			want:       false,
			wantGHCall: true,
		},
		{
			name:       "GitHub error counts as busy",
			inProgress: []string{key + "-android"},
			runsStatus: http.StatusInternalServerError,
			runsJSON:   `{}`,
			want:       false,
			wantGHCall: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newFakeCloudServer(t, "[]")
			base, ghPaths := newFakeGitHub(t, tt.runsStatus, tt.runsJSON)
			s.githubAPIBase = base
			s.e2eInProgress = map[string]bool{}
			for _, k := range tt.inProgress {
				s.e2eInProgress[k] = true
			}

			assert.Equal(t, tt.want, s.isPRE2EIdle(pr, key, key+"-android", s.Logger))
			if tt.wantGHCall {
				assert.Equal(t, []string{"GET /repos/mattermost/mattermost-mobile/actions/workflows/e2e-detox-pr.yml/runs"}, ghPaths())
			} else {
				assert.Empty(t, ghPaths())
			}
		})
	}
}

func TestHandleE2ETestRequest_IdlePRReplacesServers(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]") // creates fail, so the run stops after the create attempt
	base, _ := newFakeGitHub(t, http.StatusOK, `{"workflow_runs":[]}`)
	s.githubAPIBase = base
	s.e2eInProgress = map[string]bool{}
	s.e2ePRCleanupGeneration = map[string]int64{}

	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile", FullName: "mattermost/mattermost-mobile", Number: 5, Ref: "b", Sha: "s"}
	key := "mattermost-mobile-pr-5"
	s.e2eInstances[key] = []*E2EInstance{{InstallationID: "old-1"}, {InstallationID: "old-2"}}

	s.handleE2ETestRequest(pr, "E2E/Run")

	var deletes, creates int
	fc.mu.Lock()
	for _, r := range fc.requests {
		switch {
		case r.Method == http.MethodDelete && (r.URL.Path == "/api/installation/old-1" || r.URL.Path == "/api/installation/old-2"):
			deletes++
		case r.Method == http.MethodPost && r.URL.Path == "/api/installations":
			creates++
		}
	}
	fc.mu.Unlock()

	assert.Equal(t, 2, deletes, "the PR's old servers must be torn down")
	assert.Positive(t, creates, "fresh servers must be requested instead of reusing the old ones")
	assert.Equal(t, int64(1), s.e2ePRCleanupGeneration[key])
	require.Empty(t, s.e2eInstances[key])
}

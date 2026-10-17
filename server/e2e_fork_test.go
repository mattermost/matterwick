// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/matterwick/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsForkPR(t *testing.T) {
	tests := []struct {
		name     string
		fullName string
		want     bool
	}{
		{name: "same repo", fullName: "mattermost/mattermost-mobile", want: false},
		{name: "same repo, different case", fullName: "Mattermost/Mattermost-Mobile", want: false},
		{name: "fork", fullName: "contributor/mattermost-mobile", want: true},
		{name: "deleted fork", fullName: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile", FullName: tt.fullName}
			assert.Equal(t, tt.want, isForkPR(pr))
		})
	}
}

func TestHandleE2ETestRequest_ForkPR(t *testing.T) {
	var mu sync.Mutex
	var commentPaths []string
	var commentBodies []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var c struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(body, &c)
			commentPaths = append(commentPaths, r.URL.Path)
			commentBodies = append(commentBodies, c.Body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(gh.Close)

	for _, repo := range []string{"mattermost-mobile", "mattermost-desktop"} {
		t.Run(repo, func(t *testing.T) {
			mu.Lock()
			commentPaths, commentBodies = nil, nil
			mu.Unlock()

			s, fc := newFakeCloudServer(t, "[]")
			s.githubAPIBase = gh.URL + "/"
			pr := &model.PullRequest{
				RepoOwner: "mattermost",
				RepoName:  repo,
				FullName:  "contributor/" + repo,
				Number:    42,
				Ref:       "feature",
				Sha:       "abc123",
			}

			key := repo + "-pr-42"
			s.e2ePRCleanupGeneration = map[string]int64{}
			s.e2eInstances[key] = []*E2EInstance{{Name: "left-over", InstallationID: "left-over-id"}}

			s.handleE2ETestRequest(pr, "E2E/Run")

			deleted := false
			for _, r := range fc.requests {
				assert.NotEqual(t, http.MethodPost, r.Method, "no cloud installation may be created for a fork PR: %s", r.URL.Path)
				deleted = deleted || (r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/left-over-id"))
			}
			assert.True(t, deleted, "a tracked installation for the fork PR must be destroyed")
			assert.NotEmpty(t, fc.dnsQueries(), "existing servers for the fork PR must be cleaned up")
			assert.Empty(t, s.e2eInstances, "tracked instances for the fork PR must be dropped")
			assert.Equal(t, int64(1), s.e2ePRCleanupGeneration[key], "cleanup must invalidate any provisioning still in flight")
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, commentBodies, 1)
			assert.Equal(t, "/repos/mattermost/"+repo+"/issues/42/comments", commentPaths[0])
			assert.Equal(t, forkPRE2EComment, commentBodies[0])
		})
	}
}

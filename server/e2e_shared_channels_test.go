// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	mattermostModel "github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSharedChannelsFakes starts a fake Mattermost server and a fake provisioner. calls
// records every request to either, in order.
func newSharedChannelsFakes(t *testing.T, configStatus int) (s *Server, client *mattermostModel.Client4, calls *[]string, configPatch *string, envPatch map[string]map[string]string) {
	t.Helper()
	originalInterval := e2eInstallationPollInterval
	e2eInstallationPollInterval = time.Millisecond
	t.Cleanup(func() { e2eInstallationPollInterval = originalInterval })

	var mu sync.Mutex
	calls = &[]string{}
	configPatch = new(string)
	envPatch = map[string]map[string]string{}
	record := func(call string) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, call)
	}

	mm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("mm " + r.Method + " " + r.URL.Path)
		switch r.URL.Path {
		case "/api/v4/config/patch":
			body, _ := io.ReadAll(r.Body)
			*configPatch = string(body)
			w.WriteHeader(configStatus)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v4/system/ping":
			_, _ = w.Write([]byte(`{"status":"OK"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(mm.Close)

	polls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("cloud " + r.Method + " " + r.URL.Path)
		switch {
		case r.Method == http.MethodPut:
			var req struct {
				PriorityEnv map[string]map[string]string
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			for k, v := range req.PriorityEnv {
				envPatch[k] = v
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ID":"install-abc","State":"update-requested"}`))
		case r.Method == http.MethodGet:
			// One poll mid-restart, then stable.
			state := "stable"
			if polls == 0 {
				state = "update-in-progress"
			}
			polls++
			_, _ = w.Write([]byte(`{"ID":"install-abc","State":"` + state + `"}`))
		}
	}))
	t.Cleanup(cloud.Close)

	s = &Server{
		Config:      &MatterwickConfig{},
		CloudClient: model.NewCloudClient(cloud.URL, "", "", "", ""),
		Logger:      logrus.New(),
	}
	return s, mattermostModel.NewAPIv4Client(mm.URL), calls, configPatch, envPatch
}

func TestRestartWithSharedChannels(t *testing.T) {
	t.Run("patches flags, then restarts with a nonce, then waits for stable and ping", func(t *testing.T) {
		s, client, calls, configPatch, envPatch := newSharedChannelsFakes(t, http.StatusOK)

		require.NoError(t, s.restartWithSharedChannels(context.Background(), client, "install-abc", s.Logger))

		assert.Equal(t, []string{
			"mm PUT /api/v4/config/patch",
			"cloud PUT /api/installation/install-abc/mattermost",
			"cloud GET /api/installation/install-abc",
			"cloud GET /api/installation/install-abc",
			"mm GET /api/v4/system/ping",
		}, *calls)
		assert.JSONEq(t, `{"ConnectedWorkspacesSettings":{"EnableRemoteClusterService":true,"EnableSharedChannels":true}}`, *configPatch)
		require.Len(t, envPatch, 1, "only the nonce is sent; the provisioner merges it into the existing env")
		assert.NotEmpty(t, envPatch["MATTERWICK_E2E_RESTART_NONCE"]["value"])
	})

	t.Run("config patch failure does not restart the server", func(t *testing.T) {
		s, client, calls, _, _ := newSharedChannelsFakes(t, http.StatusForbidden)

		err := s.restartWithSharedChannels(context.Background(), client, "install-abc", s.Logger)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to patch ConnectedWorkspacesSettings")
		assert.Equal(t, []string{"mm PUT /api/v4/config/patch"}, *calls)
	})
}

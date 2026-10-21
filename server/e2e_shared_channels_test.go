// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mattermostModel "github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sharedChannelsTestInstallationID = "install-abc"

// sharedChannelsFakes records the calls that reach the fake Mattermost server and the fake
// provisioner, in the order they arrive.
type sharedChannelsFakes struct {
	mu          sync.Mutex
	calls       []string
	configPatch string
	envPatch    map[string]map[string]string
}

func (f *sharedChannelsFakes) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

type sharedChannelsFakeOpts struct {
	configStatus int    // status for PUT /api/v4/config/patch
	configJSON   string // body for GET /api/v4/config
	updateState  string // installation state returned by the update
	pollStates   []string
}

// newSharedChannelsFakes starts a fake Mattermost server and a fake provisioner and returns
// a Server wired to the provisioner plus an admin client for the Mattermost server.
func newSharedChannelsFakes(t *testing.T, opts sharedChannelsFakeOpts) (*Server, *mattermostModel.Client4, *sharedChannelsFakes) {
	t.Helper()
	originalInterval := e2eInstallationPollInterval
	e2eInstallationPollInterval = time.Millisecond
	t.Cleanup(func() { e2eInstallationPollInterval = originalInterval })

	f := &sharedChannelsFakes{}

	mm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record("mm " + r.Method + " " + r.URL.Path)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v4/config/patch":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.configPatch = string(body)
			f.mu.Unlock()
			w.WriteHeader(opts.configStatus)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/config":
			_, _ = w.Write([]byte(opts.configJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/system/ping":
			_, _ = w.Write([]byte(`{"status":"OK"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/remotecluster":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(mm.Close)

	polls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record("cloud " + r.Method + " " + r.URL.Path)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/installation/"+sharedChannelsTestInstallationID+"/mattermost":
			var req struct {
				PriorityEnv map[string]map[string]string
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.envPatch = req.PriorityEnv
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ID":"` + sharedChannelsTestInstallationID + `","State":"` + opts.updateState + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/installation/"+sharedChannelsTestInstallationID:
			state := "stable"
			if polls < len(opts.pollStates) {
				state = opts.pollStates[polls]
			}
			polls++
			_, _ = w.Write([]byte(`{"ID":"` + sharedChannelsTestInstallationID + `","State":"` + state + `"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(cloud.Close)

	s := &Server{
		Config:      &MatterwickConfig{DNSNameTestServer: "test.example.com"},
		CloudClient: model.NewCloudClient(cloud.URL, "", "", "", ""),
		Logger:      logrus.New(),
	}
	client := mattermostModel.NewAPIv4Client(mm.URL)
	client.AuthToken = "admin-token"
	client.AuthType = mattermostModel.HeaderBearer
	return s, client, f
}

const bothFlagsOnConfig = `{"ConnectedWorkspacesSettings":{"EnableRemoteClusterService":true,"EnableSharedChannels":true}}`

func TestNeedsSharedChannelsBoot(t *testing.T) {
	assert.True(t, needsSharedChannelsBoot("mobile"))
	assert.False(t, needsSharedChannelsBoot("desktop"), "desktop servers must not be restarted")
	assert.False(t, needsSharedChannelsBoot(oxideE2EInstanceType), "Oxide servers must not be restarted")
}

func TestEnableSharedChannelsAtBoot(t *testing.T) {
	t.Run("patches flags, then restarts with a nonce, then waits for stable and ping", func(t *testing.T) {
		s, client, f := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
			configStatus: http.StatusOK,
			configJSON:   bothFlagsOnConfig,
			updateState:  "update-requested",
			pollStates:   []string{"update-requested", "update-in-progress", "stable"},
		})

		require.NoError(t, s.enableSharedChannelsAtBoot(context.Background(), client, sharedChannelsTestInstallationID, s.Logger))

		assert.Equal(t, []string{
			"mm PUT /api/v4/config/patch",
			"mm GET /api/v4/config",
			"cloud PUT /api/installation/install-abc/mattermost",
			"cloud GET /api/installation/install-abc",
			"cloud GET /api/installation/install-abc",
			"cloud GET /api/installation/install-abc",
			"mm GET /api/v4/system/ping",
			"mm GET /api/v4/remotecluster",
		}, f.calls)
		assert.JSONEq(t, e2eSharedChannelsConfigPatch, f.configPatch)

		require.Len(t, f.envPatch, 1, "the patch must carry only the nonce; the provisioner merges it into the existing env")
		assert.NotEmpty(t, f.envPatch[e2eRestartNonceEnv]["value"])
		assert.False(t, strings.HasPrefix(e2eRestartNonceEnv, "MM_"), "the nonce must not look like a Mattermost setting")
	})

	t.Run("each restart uses a fresh nonce", func(t *testing.T) {
		nonce := func() string {
			s, client, f := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
				configStatus: http.StatusOK, configJSON: bothFlagsOnConfig, updateState: "update-requested",
			})
			require.NoError(t, s.enableSharedChannelsAtBoot(context.Background(), client, sharedChannelsTestInstallationID, s.Logger))
			return f.envPatch[e2eRestartNonceEnv]["value"]
		}
		assert.NotEqual(t, nonce(), nonce())
	})

	t.Run("server without ConnectedWorkspacesSettings is not restarted", func(t *testing.T) {
		s, client, f := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
			configStatus: http.StatusOK,
			configJSON:   `{"ServiceSettings":{}}`,
		})

		require.NoError(t, s.enableSharedChannelsAtBoot(context.Background(), client, sharedChannelsTestInstallationID, s.Logger))
		assert.Equal(t, []string{"mm PUT /api/v4/config/patch", "mm GET /api/v4/config"}, f.calls)
	})

	t.Run("flags not on after patch fails before any restart", func(t *testing.T) {
		s, client, f := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
			configStatus: http.StatusOK,
			configJSON:   `{"ConnectedWorkspacesSettings":{"EnableRemoteClusterService":true,"EnableSharedChannels":false}}`,
		})

		err := s.enableSharedChannelsAtBoot(context.Background(), client, sharedChannelsTestInstallationID, s.Logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EnableSharedChannels=false")
		assert.Equal(t, []string{"mm PUT /api/v4/config/patch", "mm GET /api/v4/config"}, f.calls)
	})

	t.Run("config patch failure fails before any restart", func(t *testing.T) {
		s, client, f := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
			configStatus: http.StatusForbidden,
		})

		err := s.enableSharedChannelsAtBoot(context.Background(), client, sharedChannelsTestInstallationID, s.Logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to patch ConnectedWorkspacesSettings")
		assert.Equal(t, []string{"mm PUT /api/v4/config/patch"}, f.calls)
	})

	t.Run("update that schedules no restart is an error, not a silent pass", func(t *testing.T) {
		s, client, f := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
			configStatus: http.StatusOK,
			configJSON:   bothFlagsOnConfig,
			updateState:  "stable",
		})

		err := s.enableSharedChannelsAtBoot(context.Background(), client, sharedChannelsTestInstallationID, s.Logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not schedule a restart")
		assert.NotContains(t, f.calls, "mm GET /api/v4/system/ping")
	})

	t.Run("cancelled context stops the post-restart wait", func(t *testing.T) {
		s, client, _ := newSharedChannelsFakes(t, sharedChannelsFakeOpts{
			configStatus: http.StatusOK,
			configJSON:   bothFlagsOnConfig,
			updateState:  "update-requested",
			pollStates:   []string{"update-requested"},
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := s.enableSharedChannelsAtBoot(ctx, client, sharedChannelsTestInstallationID, s.Logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "installation wait cancelled")
	})
}

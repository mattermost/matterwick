// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
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

func TestMain(m *testing.M) {
	// Replacing a failed installation backs off 15s+ in production. Keep that out of every
	// test's wall-clock; tests that check the backoff install their own recorder.
	e2eRetrySleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	os.Exit(m.Run())
}

// scriptedCloud is a fake provisioner. Each platform has a script with one outcome per
// attempt (the last one repeats):
//   - "stable": the installation comes up
//   - "creation-failed": the installation fails on its first status poll
//   - "pending": the installation never comes up (polls until cancelled)
//   - an HTTP status code such as "400": the create request itself fails
type scriptedCloud struct {
	t      *testing.T
	script map[string][]string
	// after holds a platform's first create request until each listed platform's
	// installation has been polled, so a test can fail a slot while siblings are mid-wait.
	after map[string][]string

	mu       sync.Mutex
	posts    map[string]int
	outcomes map[string]string // installation ID -> outcome
	deleted  []string
	polled   map[string]chan struct{} // closed on a platform's first status poll
}

func newScriptedCloud(t *testing.T, script map[string][]string, after map[string][]string) (*Server, *scriptedCloud) {
	t.Helper()
	sc := &scriptedCloud{
		t:        t,
		script:   script,
		after:    after,
		posts:    map[string]int{},
		outcomes: map[string]string{},
		polled:   map[string]chan struct{}{},
	}
	for p := range script {
		sc.polled[p] = make(chan struct{})
	}
	ts := httptest.NewServer(http.HandlerFunc(sc.serve))
	t.Cleanup(ts.Close)

	s := &Server{
		Config:       &MatterwickConfig{DNSNameTestServer: "test.example.com"},
		CloudClient:  model.NewCloudClient(ts.URL, "", "", "", ""),
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
	}
	return s, sc
}

func (sc *scriptedCloud) platformOf(s string) string {
	for p := range sc.script {
		if strings.Contains(s, p) {
			return p
		}
	}
	sc.t.Errorf("no scripted platform in %q", s)
	return ""
}

func (sc *scriptedCloud) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/installations":
		var req struct{ OwnerID string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := sc.platformOf(req.OwnerID)

		sc.mu.Lock()
		n := sc.posts[p]
		sc.posts[p]++
		sc.mu.Unlock()
		if n == 0 {
			for _, other := range sc.after[p] {
				select {
				case <-sc.polled[other]:
				case <-time.After(5 * time.Second):
					sc.t.Errorf("%s was never polled", other)
				}
			}
		}

		steps := sc.script[p]
		outcome := steps[len(steps)-1]
		if n < len(steps) {
			outcome = steps[n]
		}
		if code, err := strconv.Atoi(outcome); err == nil {
			w.WriteHeader(code)
			return
		}
		id := fmt.Sprintf("%s-i%d", p, n+1)
		sc.mu.Lock()
		sc.outcomes[id] = outcome
		sc.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"ID": id, "OwnerID": req.OwnerID, "State": "creation-requested"})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/installation/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/installation/")
		p := sc.platformOf(id)
		sc.mu.Lock()
		outcome := sc.outcomes[id]
		select {
		case <-sc.polled[p]:
		default:
			close(sc.polled[p])
		}
		sc.mu.Unlock()
		state := map[string]string{"stable": "stable", "creation-failed": "creation-failed", "pending": "creation-in-progress"}[outcome]
		_ = json.NewEncoder(w).Encode(map[string]string{"ID": id, "State": state})

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/installation/"):
		sc.mu.Lock()
		sc.deleted = append(sc.deleted, strings.TrimPrefix(r.URL.Path, "/api/installation/"))
		sc.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (sc *scriptedCloud) deletedIDs() []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := append([]string(nil), sc.deleted...)
	sort.Strings(out)
	return out
}

func (sc *scriptedCloud) postCounts() map[string]int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := map[string]int{}
	for p, n := range sc.posts {
		out[p] = n
	}
	return out
}

// stubProvisioningTimers makes polling instant, skips the network-bound server setup, and
// records every replacement backoff instead of waiting it out.
func stubProvisioningTimers(t *testing.T) *[]time.Duration {
	t.Helper()
	origPoll, origDelete, origInit, origSleep := e2eInstallationPollInterval, installationDeleteRetryDelay, initializeE2EServer, e2eRetrySleep
	t.Cleanup(func() {
		e2eInstallationPollInterval, installationDeleteRetryDelay, initializeE2EServer, e2eRetrySleep = origPoll, origDelete, origInit, origSleep
	})
	e2eInstallationPollInterval = time.Millisecond
	installationDeleteRetryDelay = time.Millisecond
	initializeE2EServer = func(*Server, string, string, string, logrus.FieldLogger) (*mattermostModel.Client4, error) {
		return nil, nil
	}

	var mu sync.Mutex
	sleeps := &[]time.Duration{}
	e2eRetrySleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*sleeps = append(*sleeps, d)
		mu.Unlock()
		return ctx.Err()
	}
	return sleeps
}

func oxideSlots(s *Server, platforms ...string) []e2eSlot {
	slots := make([]e2eSlot, len(platforms))
	for i, p := range platforms {
		slots[i] = e2eSlot{platform: p, nameFn: func() string {
			return e2eInstanceName(s.Config.DNSNameTestServer, oxideE2EInstanceType, "pr-1", p, e2eUniqueSuffix())
		}}
	}
	return slots
}

const (
	site1 = "android-site-1"
	site2 = "android-site-2"
	site3 = "android-site-3"
)

func TestProvisionE2EInstances(t *testing.T) {
	for _, tt := range []struct {
		name        string
		script      map[string][]string
		after       map[string][]string
		wantErr     []string // substrings of the returned error; empty = success
		wantPosts   map[string]int
		wantDeleted []string
		wantSleeps  int
	}{
		{
			name:        "failed installation is replaced while siblings keep going",
			script:      map[string][]string{site1: {"stable"}, site2: {"creation-failed", "stable"}, site3: {"stable"}},
			wantPosts:   map[string]int{site1: 1, site2: 2, site3: 1},
			wantDeleted: []string{"android-site-2-i1"},
			wantSleeps:  1,
		},
		{
			name:   "slot that exhausts its attempts is the reported cause, not a sibling's cancellation",
			script: map[string][]string{site1: {"pending"}, site2: {"stable"}, site3: {"creation-failed"}},
			after:  map[string][]string{site3: {site1, site2}},
			wantErr: []string{
				"android site 3 server: failed after 3 attempts",
				"android-site-3-i3",
				`installation entered state "creation-failed"`,
			},
			wantPosts:   map[string]int{site1: 1, site2: 1, site3: createCloudInstallationAttempts},
			wantDeleted: []string{"android-site-1-i1", "android-site-2-i1", "android-site-3-i1", "android-site-3-i2", "android-site-3-i3"},
			wantSleeps:  createCloudInstallationAttempts - 1,
		},
		{
			name:        "non-retryable provisioner error fails at once and cancels siblings",
			script:      map[string][]string{site1: {"pending"}, site2: {"400"}, site3: {"pending"}},
			after:       map[string][]string{site2: {site1, site3}},
			wantErr:     []string{"android site 2 server: not retryable", "failed with status code 400"},
			wantPosts:   map[string]int{site1: 1, site2: 1, site3: 1},
			wantDeleted: []string{"android-site-1-i1", "android-site-3-i1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sleeps := stubProvisioningTimers(t)
			s, sc := newScriptedCloud(t, tt.script, tt.after)

			instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, site1, site2, site3), "11.9.0", "admin", "pw", oxideE2EInstanceType, s.Logger)

			if len(tt.wantErr) == 0 {
				require.NoError(t, err)
				require.Len(t, instances, 3)
				for i, p := range []string{site1, site2, site3} {
					assert.Equal(t, p, instances[i].Platform, "instances must come back in slot order")
				}
				assert.Equal(t, "android-site-2-i"+strconv.Itoa(tt.wantPosts[site2]), instances[1].InstallationID)
			} else {
				require.Error(t, err)
				assert.Nil(t, instances)
				for _, want := range tt.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				assert.NotContains(t, err.Error(), "context canceled", "a sibling's cancellation must never be reported as the cause")
				assert.False(t, errors.Is(err, context.Canceled))
			}

			assert.Equal(t, tt.wantPosts, sc.postCounts())
			assert.Equal(t, tt.wantDeleted, sc.deletedIDs(), "every failed or abandoned installation is deleted, and only those")

			require.Len(t, *sleeps, tt.wantSleeps)
			for i, d := range *sleeps {
				base := e2eInstallationRetryBackoff[i]
				assert.True(t, d >= base && d <= base+base/5, "backoff before attempt %d is %s, want %s plus up to 20%% jitter", i+2, d, base)
			}
		})
	}
}

func TestProvisionE2EInstances_CallerCancels(t *testing.T) {
	stubProvisioningTimers(t)
	s, sc := newScriptedCloud(t, map[string][]string{site1: {"pending"}, site2: {"pending"}, site3: {"pending"}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for _, p := range []string{site1, site2, site3} {
			<-sc.polled[p]
		}
		cancel()
	}()

	instances, err := s.provisionE2EInstances(ctx, oxideSlots(s, site1, site2, site3), "11.9.0", "admin", "pw", oxideE2EInstanceType, s.Logger)

	require.Error(t, err)
	assert.Nil(t, instances)
	assert.True(t, errors.Is(err, context.Canceled), "only a caller cancellation is reported as a cancellation: %v", err)
	assert.Equal(t, []string{"android-site-1-i1", "android-site-2-i1", "android-site-3-i1"}, sc.deletedIDs())
}

func TestProvisionE2EInstances_SlotTimeCap(t *testing.T) {
	stubProvisioningTimers(t)
	orig := e2eInstallationMaxDuration
	e2eInstallationMaxDuration = 50 * time.Millisecond
	t.Cleanup(func() { e2eInstallationMaxDuration = orig })
	s, sc := newScriptedCloud(t, map[string][]string{site1: {"stable"}, site2: {"pending"}}, nil)

	instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, site1, site2), "11.9.0", "admin", "pw", oxideE2EInstanceType, s.Logger)

	require.Error(t, err)
	assert.Nil(t, instances)
	assert.Contains(t, err.Error(), "android site 2 server: gave up after 50ms (1 attempts)")
	assert.False(t, errors.Is(err, context.Canceled))
	assert.Equal(t, []string{"android-site-1-i1", "android-site-2-i1"}, sc.deletedIDs())
}

func TestIsRetryableE2EInstallationError(t *testing.T) {
	for _, tt := range []struct {
		err  string
		want bool
	}{
		{"failed to create installation x: failed with status code 500", true},
		{"failed to create installation x: failed with status code 409", true},
		{"failed to create installation x: failed with status code 429", true},
		{"failed to create installation x: failed with status code 400", false},
		{"installation i (x): failed to get installation status: failed with status code 403", false},
		{`installation i (x): installation entered state "creation-failed"`, true},
	} {
		t.Run(tt.err, func(t *testing.T) {
			assert.Equal(t, tt.want, isRetryableE2EInstallationError(errors.New(tt.err)))
		})
	}
}

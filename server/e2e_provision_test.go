// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
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
	installationDeleteRetryDelay = time.Millisecond
	os.Exit(m.Run())
}

// scriptedCloud is a fake provisioner. Each platform has a script with one outcome per
// attempt (the last one repeats):
//   - "stable": the installation comes up
//   - "creation-failed": the installation fails on its first status poll
//   - "pending": the installation never comes up (polls until cancelled)
//   - "get403": the installation is created, but its status poll answers 403
//   - "capacity": the first 3 polls report no cluster has room, then it is stable
//   - "hang": its status polls never answer
//   - an HTTP status code such as "400": the create request itself fails
type scriptedCloud struct {
	t      *testing.T
	script map[string][]string
	// after holds a platform's first status poll until each listed platform's installation
	// has been polled, so a test can fail a slot while its siblings are mid-wait. (Not the
	// create: creates are sent one at a time.)
	after map[string][]string

	mu       sync.Mutex
	posts    map[string]int
	outcomes map[string]string // installation ID -> outcome
	deleted  []string
	polled   map[string]chan struct{} // closed on a platform's first status poll
	polls    map[string]int           // installation ID -> status polls answered
	release  chan struct{}            // closed at cleanup, ending "hang" polls
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
		polls:    map[string]int{},
		release:  make(chan struct{}),
	}
	for p := range script {
		sc.polled[p] = make(chan struct{})
	}
	ts := httptest.NewServer(http.HandlerFunc(sc.serve))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(sc.release) }) // runs before ts.Close, so hung polls can end

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
		sc.polls[id]++
		polls := sc.polls[id]
		first := false
		select {
		case <-sc.polled[p]:
		default:
			close(sc.polled[p])
			first = true
		}
		sc.mu.Unlock()
		if first {
			for _, other := range sc.after[p] {
				select {
				case <-sc.polled[other]:
				case <-time.After(5 * time.Second):
					sc.t.Errorf("%s was never polled", other)
				}
			}
		}
		switch {
		case outcome == "get403":
			w.WriteHeader(http.StatusForbidden)
			return
		case outcome == "hang":
			<-sc.release
			return
		case outcome == "capacity" && polls <= 3:
			_ = json.NewEncoder(w).Encode(map[string]string{"ID": id, "State": "creation-no-compatible-clusters"})
			return
		case outcome == "capacity":
			outcome = "stable"
		}
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

// Provisioning tests use the desktop kind: mobile kinds also restart the server for Shared
// Channels, which needs a real Mattermost client.

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
			return e2eInstanceName(s.Config.DNSNameTestServer, oxideE2EProfile.kind, "pr-1", p, e2eUniqueSuffix())
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
			script:      map[string][]string{site1: {"pending"}, site2: {"get403"}, site3: {"pending"}},
			after:       map[string][]string{site2: {site1, site3}},
			wantErr:     []string{"android site 2 server: not retryable", "failed with status code 403"},
			wantPosts:   map[string]int{site1: 1, site2: 1, site3: 1},
			wantDeleted: []string{"android-site-1-i1", "android-site-2-i1", "android-site-3-i1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sleeps := stubProvisioningTimers(t)
			s, sc := newScriptedCloud(t, tt.script, tt.after)

			instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, site1, site2, site3), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

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

	instances, err := s.provisionE2EInstances(ctx, oxideSlots(s, site1, site2, site3), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

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

	instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, site1, site2), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

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

// groupLockedCloud emulates the provisioner with CloudGroupID set: each create takes a
// non-blocking lock on the group while it inserts the installation, and answers 409 if the
// lock is already held, by another create or (supervisorHolds) by the group supervisor.
type groupLockedCloud struct {
	group sync.Mutex

	mu              sync.Mutex
	supervisorHolds int
	conflicts       int
	requested       []string // DNS of every create request
	deleted         []string
}

func newGroupLockedCloud(t *testing.T, supervisorHolds int) (*Server, *groupLockedCloud) {
	t.Helper()
	gc := &groupLockedCloud{supervisorHolds: supervisorHolds}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/installations":
			var req struct{ DNSNames []string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			gc.mu.Lock()
			gc.requested = append(gc.requested, strings.Join(req.DNSNames, ","))
			n := len(gc.requested)
			held := gc.supervisorHolds > 0
			if held {
				gc.supervisorHolds--
			}
			gc.mu.Unlock()
			if held || !gc.group.TryLock() {
				gc.mu.Lock()
				gc.conflicts++
				gc.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				return
			}
			time.Sleep(20 * time.Millisecond) // the insert, under the group lock
			gc.group.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"ID": fmt.Sprintf("i%d", n), "State": "creation-requested"})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"ID": strings.TrimPrefix(r.URL.Path, "/api/installation/"), "State": "stable"})
		case r.Method == http.MethodDelete:
			gc.mu.Lock()
			gc.deleted = append(gc.deleted, strings.TrimPrefix(r.URL.Path, "/api/installation/"))
			gc.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return &Server{
		Config:      &MatterwickConfig{DNSNameTestServer: "test.example.com"},
		CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""),
		Logger:      logrus.New(),
	}, gc
}

// Regression for desktop#4040 / Oxide#360: a set's creates were sent at once, all but one got
// 409 from the group lock, and the run failed within seconds as "context canceled".
func TestProvisionE2EInstances_GroupLockedCreatesDoNotConflict(t *testing.T) {
	sleeps := stubProvisioningTimers(t)
	s, gc := newGroupLockedCloud(t, 0)

	instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, oxideE2EPlatforms...), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

	require.NoError(t, err)
	require.Len(t, instances, len(oxideE2EPlatforms))
	assert.Zero(t, gc.conflicts, "a set's own creates must never collide on the group lock")
	assert.Len(t, gc.requested, len(oxideE2EPlatforms), "one create per server")
	assert.Empty(t, gc.deleted)
	assert.Empty(t, *sleeps)
}

func TestProvisionE2EInstances_GroupLockedBySupervisorIsRetried(t *testing.T) {
	sleeps := stubProvisioningTimers(t)
	s, gc := newGroupLockedCloud(t, 2)

	instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, site1), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

	require.NoError(t, err)
	require.Len(t, instances, 1)
	require.Len(t, gc.requested, 3)
	assert.Equal(t, gc.requested[0], gc.requested[2], "nothing was created, so the same request is re-sent")
	assert.Equal(t, []time.Duration{2 * time.Second, 5 * time.Second}, *sleeps, "a locked group is retried quickly, not replaced after the 15s backoff")
	assert.Empty(t, gc.deleted)
}

func TestProvisionE2EInstances_QueueBoundsConcurrentCreation(t *testing.T) {
	stubProvisioningTimers(t)
	script := map[string][]string{}
	for _, p := range oxideE2EPlatforms {
		script[p] = []string{"stable"}
	}
	s, _ := newScriptedCloud(t, script, nil)
	s.Config.E2EMaxConcurrentProvisions = 2

	var mu sync.Mutex
	active, peak := 0, 0
	initializeE2EServer = func(*Server, string, string, string, logrus.FieldLogger) (*mattermostModel.Client4, error) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return nil, nil
	}

	instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, oxideE2EPlatforms...), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

	require.NoError(t, err)
	assert.Len(t, instances, len(oxideE2EPlatforms), "every requested server is provided, the excess just waits")
	assert.Equal(t, 2, peak, "no more than E2EMaxConcurrentProvisions servers are in creation at once")
}

func TestProvisionE2EInstances_WaitsForClusterCapacity(t *testing.T) {
	stubProvisioningTimers(t)
	orig := e2eInstallationStableTimeout
	e2eInstallationStableTimeout = 2 * time.Millisecond
	t.Cleanup(func() { e2eInstallationStableTimeout = orig })
	e2eInstallationPollInterval = 5 * time.Millisecond // 3 capacity polls outlast the stability timeout
	s, sc := newScriptedCloud(t, map[string][]string{site1: {"capacity"}}, nil)

	instances, err := s.provisionE2EInstances(context.Background(), oxideSlots(s, site1), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

	require.NoError(t, err, "waiting for a cluster with room must not count as failing to become stable")
	require.Len(t, instances, 1)
	assert.Empty(t, sc.deletedIDs(), "a server waiting for capacity is kept, not replaced")
}

func TestProvisionE2EInstances_HungStatusPollDoesNotHangTheRun(t *testing.T) {
	stubProvisioningTimers(t)
	orig := e2eCloudCallTimeout
	e2eCloudCallTimeout = 20 * time.Millisecond
	t.Cleanup(func() { e2eCloudCallTimeout = orig })
	s, sc := newScriptedCloud(t, map[string][]string{site1: {"hang", "stable"}, site2: {"stable"}}, nil)

	done := make(chan error, 1)
	var instances []*E2EInstance
	go func() {
		var err error
		instances, err = s.provisionE2EInstances(context.Background(), oxideSlots(s, site1, site2), "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a provisioner request that never answers hung the run")
	}
	require.Len(t, instances, 2)
	assert.Equal(t, []string{"android-site-1-i1"}, sc.deletedIDs(), "the unreachable installation is deleted and replaced")
}

func TestSendCreateInstallation_Cancellation(t *testing.T) {
	stubProvisioningTimers(t)
	posts := make(chan *http.Request, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // read the body, so the server notices the client hanging up
		posts <- r
		<-r.Context().Done() // never answers; ends when the client aborts
	}))
	t.Cleanup(ts.Close)
	s := &Server{Config: &MatterwickConfig{}, CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

	t.Run("an in-flight create is aborted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-posts; cancel() }()
		_, err := s.sendCreateInstallation(ctx, &cloudModel.CreateInstallationRequest{OwnerID: "x"}, s.Logger)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("a cancelled caller waiting for its turn sends nothing", func(t *testing.T) {
		gate := s.createGate()
		gate <- struct{}{} // another create holds the turn
		t.Cleanup(func() { <-gate })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := s.sendCreateInstallation(ctx, &cloudModel.CreateInstallationRequest{OwnerID: "y"}, s.Logger)
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, posts, "no create request may be sent")
	})
}

func TestDeleteInstallationWithRetry(t *testing.T) {
	for _, tt := range []struct {
		name      string
		responses []int
		wantCalls int
	}{
		{"provisioner busy with the installation (409) is retried", []int{409, 409, 202}, 3},
		{"already gone (404) counts as deleted", []int{404}, 1},
		{"persistent failure uses every attempt", []int{500}, installationDeleteAttempts},
		{"already being deleted (400) is not retried", []int{400}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet { // state check after a 400
					_ = json.NewEncoder(w).Encode(map[string]string{"ID": "i1", "State": "deletion-requested"})
					return
				}
				mu.Lock()
				i := calls
				calls++
				mu.Unlock()
				if i >= len(tt.responses) {
					i = len(tt.responses) - 1
				}
				w.WriteHeader(tt.responses[i])
			}))
			t.Cleanup(ts.Close)
			s := &Server{Config: &MatterwickConfig{}, CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

			s.deleteInstallationWithRetry("i1", "name", s.Logger)

			assert.Equal(t, tt.wantCalls, calls)
		})
	}
}

func TestCreateCloudInstallation_UnansweredCreateIsCleanedUp(t *testing.T) {
	stubProvisioningTimers(t)
	var mu sync.Mutex
	var created, deleted, requests []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			var req struct {
				Name, DNS string
				DNSNames  []string
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			created = append(created, strings.Join(req.DNSNames, ","))
			requests = append(requests, req.Name+"|"+req.DNS)
			mu.Unlock()
			// The provisioner creates it, then the connection drops before the 202 arrives.
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/api/installations":
			mu.Lock()
			found := len(created) > 0 && r.URL.Query().Get("dns_name") == created[0]
			mu.Unlock()
			if found {
				_, _ = w.Write([]byte(`[{"ID":"ghost","OwnerID":"x","State":"creation-requested"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/api/installation/"))
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(ts.Close)
	s := &Server{Config: &MatterwickConfig{DNSNameTestServer: "test.example.com"}, CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

	_, err := s.createCloudInstallation(context.Background(), "oxide-pr-1-x-abcdef12", "11.9.0", "admin", "pw", desktopE2EProfile.kind, s.Logger)

	require.Error(t, err)
	assert.Equal(t, []string{"ghost"}, deleted, "an installation created without an answer must be found and deleted")
	assert.Equal(t, []string{"oxide-pr-1-x-abcdef12.test.example.com"}, created, "the name goes in DNSNames")
	assert.Equal(t, []string{"oxide-pr-1-x-abcdef12|"}, requests, "Name is set and the deprecated DNS field is not")
}

func TestWakeUpHibernatingInstances_GoneServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // the client returns nil, nil
	}))
	t.Cleanup(ts.Close)
	s := &Server{Config: &MatterwickConfig{}, CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

	assert.False(t, s.wakeUpHibernatingInstances([]*E2EInstance{{InstallationID: "gone"}}, s.Logger),
		"a deleted server must be reported, not dereferenced (this used to panic and crash matterwick)")
}

// stubWakeTimeout lets a wait for a server that never serves end quickly.
func stubWakeTimeout(t *testing.T) {
	t.Helper()
	orig := e2eWakeTimeout
	t.Cleanup(func() { e2eWakeTimeout = orig })
	e2eWakeTimeout = 50 * time.Millisecond
}

func TestWakeUpHibernatingInstances_OnlyServingServersAreReused(t *testing.T) {
	stubProvisioningTimers(t)
	stubWakeTimeout(t)
	for _, tc := range []struct {
		name   string
		states []string // GET answers in turn, the last repeating; "404" and "500" are statuses
		wake   int
		want   bool
	}{
		{"stable", []string{"stable"}, 0, true},
		{"woken", []string{"hibernating", "wake-up-requested", "update-in-progress", "stable"}, http.StatusAccepted, true},
		{"brief state-check error", []string{"500", "stable"}, 0, true},
		{"going to sleep, then woken", []string{"hibernation-in-progress", "hibernating", "stable"}, http.StatusAccepted, true},
		{"state check never answers", []string{"500"}, 0, false},
		{"wake-up refused", []string{"hibernating"}, http.StatusBadGateway, false},
		{"wake-up fails", []string{"hibernating", "update-failed"}, http.StatusAccepted, false},
		{"deleted while waking", []string{"hibernating", "404"}, http.StatusAccepted, false},
		{"deleted", []string{"deleted"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			gets := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(tc.wake)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				mu.Lock()
				state := tc.states[min(gets, len(tc.states)-1)]
				gets++
				mu.Unlock()
				switch state {
				case "404":
					w.WriteHeader(http.StatusNotFound)
				case "500":
					w.WriteHeader(http.StatusInternalServerError)
				default:
					_, _ = w.Write([]byte(`{"ID":"i","State":"` + state + `"}`))
				}
			}))
			t.Cleanup(ts.Close)
			s := &Server{Config: &MatterwickConfig{}, CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

			assert.Equal(t, tc.want, s.wakeUpHibernatingInstances([]*E2EInstance{{InstallationID: "i"}}, s.Logger))
		})
	}
}

func TestWakeUpHibernatingInstances_ADeadServerStopsTheOthersWaits(t *testing.T) {
	stubProvisioningTimers(t) // e2eWakeTimeout stays 10 minutes: only the dead server may end this
	wakeSent, wakeCancelled := make(chan struct{}), make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/installation/dead":
			<-wakeSent // the other server is mid wake-up when this one turns out dead
			_, _ = w.Write([]byte(`{"ID":"dead","State":"deleted"}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"ID":"cold","State":"hibernating"}`))
		default: // the wake-up never answers, until its request is cancelled
			_, _ = io.Copy(io.Discard, r.Body) // the server notices a dropped client only after the body
			close(wakeSent)
			<-r.Context().Done()
			close(wakeCancelled)
		}
	}))
	t.Cleanup(ts.Close)
	s := &Server{Config: &MatterwickConfig{}, CloudClient: model.NewCloudClient(ts.URL, "", "", "", ""), Logger: logrus.New()}

	done := make(chan bool)
	go func() {
		done <- s.wakeUpHibernatingInstances([]*E2EInstance{{InstallationID: "cold"}, {InstallationID: "dead"}}, s.Logger)
	}()

	select {
	case serving := <-done:
		assert.False(t, serving)
	case <-time.After(10 * time.Second):
		t.Fatal("a dead server must stop the wait on the others")
	}
	select {
	case <-wakeCancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the hung wake-up request must be cancelled, not left running")
	}
}

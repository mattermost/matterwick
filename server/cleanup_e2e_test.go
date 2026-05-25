// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

// newCleanupE2ETestServer builds a Server for handleCleanupE2E. CloudClient
// is deliberately nil; callers must avoid paths that trigger the destroy
// goroutine. The "instances tracked" path is covered indirectly by
// asserting on the e2eInstances map state and the synchronous response,
// and by giving the tracked entry an empty []*E2EInstance slice so the
// goroutine sees no work and exits without touching CloudClient.
func newCleanupE2ETestServer(t *testing.T, cleanupSecret string) *Server {
	t.Helper()
	return &Server{
		Config: &MatterwickConfig{
			CleanupSecret: cleanupSecret,
		},
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
	}
}

const validCleanupBody = `{"repo": "desktop", "run_id": 26026866029}`

func doCleanupE2ERequest(t *testing.T, s *Server, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/cleanup_e2e", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Cleanup-Token", token)
	}
	w := httptest.NewRecorder()
	s.handleCleanupE2E(w, req)
	return w
}

func TestHandleCleanupE2E_RejectsWhenSecretUnconfigured(t *testing.T) {
	s := newCleanupE2ETestServer(t, "")
	w := doCleanupE2ERequest(t, s, validCleanupBody, "any-token")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestHandleCleanupE2E_RejectsMissingToken(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	w := doCleanupE2ERequest(t, s, validCleanupBody, "")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandleCleanupE2E_RejectsWrongToken(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	w := doCleanupE2ERequest(t, s, validCleanupBody, "not-the-secret")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandleCleanupE2E_RejectsMalformedJSON(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	w := doCleanupE2ERequest(t, s, `{not json`, "the-secret")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCleanupE2E_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing repo", `{"run_id":12345}`},
		{"missing run_id", `{"repo":"desktop"}`},
		{"zero run_id", `{"repo":"desktop","run_id":0}`},
		{"empty repo", `{"repo":"","run_id":12345}`},
	}
	s := newCleanupE2ETestServer(t, "the-secret")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doCleanupE2ERequest(t, s, tc.body, "the-secret")
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"missing field %q must yield 400", tc.name)
		})
	}
}

func TestHandleCleanupE2E_NoMatchingInstances(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	// Map is empty. Cleanup should return 200 "no_instances" without panicking
	// and without launching the destroy goroutine.
	w := doCleanupE2ERequest(t, s, validCleanupBody, "the-secret")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "no_instances",
		"empty map must yield no_instances response")
}

func TestHandleCleanupE2E_NonMatchingKeyIgnored(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	// Pre-populate the map with a key that does NOT match the cleanup
	// request's prefix. The unrelated entry must be left untouched.
	otherKey := "desktop-pr-9999"
	s.e2eInstances[otherKey] = []*E2EInstance{{InstallationID: "other-inst"}}

	w := doCleanupE2ERequest(t, s, validCleanupBody, "the-secret")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "no_instances")
	assert.Contains(t, s.e2eInstances, otherKey,
		"non-matching tracking key must not be deleted")
}

func TestHandleCleanupE2E_RemovesMatchingKey(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	// Tracking key format from handleCMTWithServerVersions:
	//   "{repo}-cmt-{runID}-{sha}"
	// We populate it with an EMPTY instances slice so the goroutine has no
	// work to do (and therefore does not touch the nil CloudClient).
	matchingKey := "desktop-cmt-26026866029-abc123def456"
	s.e2eInstances[matchingKey] = []*E2EInstance{}

	w := doCleanupE2ERequest(t, s, validCleanupBody, "the-secret")

	assert.Equal(t, http.StatusOK, w.Code,
		"matching key must be removed and 200 returned")
	assert.NotContains(t, s.e2eInstances, matchingKey,
		"matching tracking key must be deleted from e2eInstances map")
}

func TestHandleCleanupE2E_RemovesMultipleMatchingKeys(t *testing.T) {
	s := newCleanupE2ETestServer(t, "the-secret")
	// Two CMT entries for the same run_id (e.g. retried provisioning) -- both
	// share the same {repo}-cmt-{run_id}- prefix. Both must be cleaned.
	k1 := "desktop-cmt-26026866029-sha-aaa"
	k2 := "desktop-cmt-26026866029-sha-bbb"
	kOther := "desktop-cmt-99999999999-sha-ccc" // different run_id, must survive
	s.e2eInstances[k1] = []*E2EInstance{}
	s.e2eInstances[k2] = []*E2EInstance{}
	s.e2eInstances[kOther] = []*E2EInstance{}

	w := doCleanupE2ERequest(t, s, validCleanupBody, "the-secret")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, s.e2eInstances, k1)
	assert.NotContains(t, s.e2eInstances, k2)
	assert.Contains(t, s.e2eInstances, kOther,
		"unrelated run_id entry must remain after cleanup")
}

func TestHandleCleanupE2E_ConstantTimeTokenCompare(t *testing.T) {
	s := newCleanupE2ETestServer(t, "cleanup-secret-abcdef")
	w := doCleanupE2ERequest(t, s, validCleanupBody, "cleanup-secret-ZZZZZZ")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

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

// newCMTDispatchTestServer builds a Server suitable for exercising
// handleCMTDispatch's synchronous validation logic. It deliberately does NOT
// set CloudClient, so callers must not exercise paths that launch the
// provisioning goroutine (i.e. only the rejection / 4xx cases). Tests that
// need the goroutine to run must build their own server with a mocked
// CloudClient -- see e2e_dryrun_test.go for that pattern.
func newCMTDispatchTestServer(t *testing.T, triggerSecret string) *Server {
	t.Helper()
	return &Server{
		Config: &MatterwickConfig{
			Org:                "mattermost",
			GithubAccessToken:  "test-token",
			CMTTriggerSecret:   triggerSecret,
			E2EServerVersion:   "latest",
		},
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
	}
}

// validCMTDispatchBody is the canonical happy-path body. Individual tests
// rebuild it with one field tweaked to exercise validation branches.
const validCMTDispatchBody = `{
  "owner": "mattermost",
  "repo": "desktop",
  "sha": "abc123def456",
  "ref": "master",
  "run_id": 26026866029,
  "server_versions": "v11.1.0, v11.2.0"
}`

func doCMTDispatchRequest(t *testing.T, s *Server, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/cmt_dispatch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Trigger-Token", token)
	}
	w := httptest.NewRecorder()
	s.handleCMTDispatch(w, req)
	return w
}

func TestHandleCMTDispatch_RejectsWhenSecretUnconfigured(t *testing.T) {
	// Refuses to run unauthenticated even if a caller presents some token,
	// because there is no configured secret to compare against.
	s := newCMTDispatchTestServer(t, "")
	w := doCMTDispatchRequest(t, s, validCMTDispatchBody, "any-token")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"unconfigured secret must yield 503, not 200/202/401/403")
}

func TestHandleCMTDispatch_RejectsMissingToken(t *testing.T) {
	s := newCMTDispatchTestServer(t, "the-secret")
	w := doCMTDispatchRequest(t, s, validCMTDispatchBody, "")
	assert.Equal(t, http.StatusForbidden, w.Code,
		"absent X-Trigger-Token header must yield 403")
}

func TestHandleCMTDispatch_RejectsWrongToken(t *testing.T) {
	s := newCMTDispatchTestServer(t, "the-secret")
	w := doCMTDispatchRequest(t, s, validCMTDispatchBody, "not-the-secret")
	assert.Equal(t, http.StatusForbidden, w.Code,
		"wrong X-Trigger-Token must yield 403")
}

func TestHandleCMTDispatch_RejectsMalformedJSON(t *testing.T) {
	s := newCMTDispatchTestServer(t, "the-secret")
	w := doCMTDispatchRequest(t, s, `{not valid json`, "the-secret")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"malformed body must yield 400 after token passes")
}

func TestHandleCMTDispatch_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing owner", `{"repo":"desktop","sha":"a","ref":"master","run_id":1,"server_versions":"v11.0.0"}`},
		{"missing repo", `{"owner":"m","sha":"a","ref":"master","run_id":1,"server_versions":"v11.0.0"}`},
		{"missing sha", `{"owner":"m","repo":"desktop","ref":"master","run_id":1,"server_versions":"v11.0.0"}`},
		{"missing ref", `{"owner":"m","repo":"desktop","sha":"a","run_id":1,"server_versions":"v11.0.0"}`},
		{"zero run_id", `{"owner":"m","repo":"desktop","sha":"a","ref":"master","run_id":0,"server_versions":"v11.0.0"}`},
		{"empty server_versions", `{"owner":"m","repo":"desktop","sha":"a","ref":"master","run_id":1,"server_versions":""}`},
	}
	s := newCMTDispatchTestServer(t, "the-secret")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doCMTDispatchRequest(t, s, tc.body, "the-secret")
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"missing required field %q must yield 400", tc.name)
		})
	}
}

func TestHandleCMTDispatch_RejectsServerVersionsThatParseToEmpty(t *testing.T) {
	s := newCMTDispatchTestServer(t, "the-secret")
	// parseServerVersionsFromString collapses ",,," into the empty slice.
	body := `{"owner":"m","repo":"desktop","sha":"a","ref":"master","run_id":1,"server_versions":",,,"}`
	w := doCMTDispatchRequest(t, s, body, "the-secret")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"server_versions that parses to zero versions must yield 400")
}

func TestHandleCMTDispatch_RejectsUnknownRepoType(t *testing.T) {
	s := newCMTDispatchTestServer(t, "the-secret")
	// Repo name contains neither "desktop" nor "mobile".
	body := `{"owner":"m","repo":"playbooks","sha":"a","ref":"master","run_id":1,"server_versions":"v11.0.0"}`
	w := doCMTDispatchRequest(t, s, body, "the-secret")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"unsupported repo must yield 400, never start provisioning")
}

func TestHandleCMTDispatch_ConstantTimeTokenCompare(t *testing.T) {
	// Sanity check: the comparison must not short-circuit on prefix match.
	// (We can't directly observe timing in a unit test, but we can at least
	// confirm that a partial-match token of the same length is still rejected.)
	s := newCMTDispatchTestServer(t, "secret-abcdef")
	w := doCMTDispatchRequest(t, s, validCMTDispatchBody, "secret-ZZZZZZ")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// NOTE: we deliberately do not test the "happy path returns 202" case in this
// file. A valid request launches the provisioning goroutine, which calls into
// CloudClient (nil in this test harness) and panics. End-to-end behavior of
// handleCMTWithServerVersions is covered by e2e_dryrun_test.go with a mocked
// cloud client. The synchronous validation logic above (rejection branches +
// missing-field detection) covers the bug-prone surface of this handler.

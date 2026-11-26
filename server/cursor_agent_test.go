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
	"strconv"
	"sync"
	"testing"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/internal/spinwick"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cursorTestDesignCheck  = "Cursor Automation: dkh-prototype-design"
	cursorTestExecuteCheck = "Cursor Automation: dkh-prototype-execute"

	cursorTestAgentA = "bc-3450c942-e7b6-4542-808e-d625454cd4c6"
	cursorTestAgentB = "bc-7c6b915d-b36f-4cae-adee-443c8aeb29d6"
	cursorTestOwner  = "owner-id"
	cursorTestSender = "dylan"
)

var cursorTestLabelTime = time.Date(2026, 11, 26, 14, 25, 54, 0, time.UTC)

type cursorFakeComment struct {
	Author  string
	Body    string
	Created time.Time
}

type cursorFakeCheckRun struct {
	Name       string
	AppSlug    string
	DetailsURL string
}

// cursorFakeGitHub serves the few GitHub endpoints the Cursor agent lookup and the public comment use.
type cursorFakeGitHub struct {
	srv *httptest.Server

	mu           sync.Mutex
	commentPages [][]cursorFakeComment
	checkRuns    []cursorFakeCheckRun
	posted       []string
}

func newCursorFakeGitHub(t *testing.T) *cursorFakeGitHub {
	t.Helper()
	f := &cursorFakeGitHub{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *cursorFakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/repos/mattermost/mattermost/issues/7/comments" && r.Method == http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.posted = append(f.posted, body.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))

	case r.URL.Path == "/repos/mattermost/mattermost/issues/7/comments" && r.Method == http.MethodGet:
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		if page < 1 || page > len(f.commentPages) {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if page < len(f.commentPages) {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, f.srv.URL, r.URL.Path, page+1))
		}
		var out []map[string]interface{}
		for i, c := range f.commentPages[page-1] {
			out = append(out, map[string]interface{}{
				"id":         page*100 + i,
				"body":       c.Body,
				"user":       map[string]string{"login": c.Author},
				"created_at": c.Created.UTC().Format(time.RFC3339),
			})
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.URL.Path == "/repos/mattermost/mattermost/commits/abc123/check-runs" && r.Method == http.MethodGet:
		name := r.URL.Query().Get("check_name")
		var runs []map[string]interface{}
		for _, c := range f.checkRuns {
			if name != "" && c.Name != name {
				continue
			}
			runs = append(runs, map[string]interface{}{
				"name":        c.Name,
				"app":         map[string]string{"slug": c.AppSlug},
				"details_url": c.DetailsURL,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"total_count": len(runs), "check_runs": runs})

	default:
		http.NotFound(w, r)
	}
}

func (f *cursorFakeGitHub) publicComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posted...)
}

func cursorMarkerComment(agentID string) string {
	return "Requesting a test environment from Matterwick.\n<!-- cursor-agent-request: " + agentID + " -->"
}

func cursorAgentLinkURL(agentID string) string {
	return "https://cursor.com/agents/" + agentID
}

func cursorTestServer(gh *cursorFakeGitHub, cfg *MatterwickConfig) *Server {
	if cfg.CursorAPIKey == "" && cfg.CursorAPIBaseURL == "" {
		cfg.CursorAPIKey = "cursor-key"
	}
	if len(cfg.CursorAgentCheckNames) == 0 {
		cfg.CursorAgentCheckNames = []string{cursorTestDesignCheck, cursorTestExecuteCheck}
	}
	return &Server{Config: cfg, Logger: logrus.New(), githubAPIBase: gh.srv.URL + "/"}
}

func cursorTestPR() *model.PullRequest {
	return &model.PullRequest{
		RepoOwner: "mattermost",
		RepoName:  "mattermost",
		Number:    7,
		Sha:       "abc123",
		HTMLURL:   "https://github.com/mattermost/mattermost/pull/7",
	}
}

func cursorTestInstallation() *cloudModel.InstallationDTO {
	return &cloudModel.InstallationDTO{
		Installation: &cloudModel.Installation{ID: "install-id"},
		DNSRecords:   []*cloudModel.InstallationDNS{{DomainName: "pr-7.test.example.com"}},
	}
}

func TestFindRequestingCursorAgent(t *testing.T) {
	recent := cursorTestLabelTime.Add(-4 * time.Second)

	for _, tc := range []struct {
		name     string
		pages    [][]cursorFakeComment
		checks   []cursorFakeCheckRun
		sender   string
		expected string
	}{
		{
			name:     "marker and matching check",
			pages:    [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}},
			checks:   []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender:   cursorTestSender,
			expected: cursorTestAgentA,
		},
		{
			name:     "sender comparison ignores case",
			pages:    [][]cursorFakeComment{{{"Dylan", cursorMarkerComment(cursorTestAgentA), recent}}},
			checks:   []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender:   "dylan",
			expected: cursorTestAgentA,
		},
		{
			name:     "second configured check name",
			pages:    [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentB), recent}}},
			checks:   []cursorFakeCheckRun{{cursorTestExecuteCheck, "cursor", cursorAgentLinkURL(cursorTestAgentB)}},
			sender:   cursorTestSender,
			expected: cursorTestAgentB,
		},
		{
			name:   "marker posted by someone else",
			pages:  [][]cursorFakeComment{{{"intruder", cursorMarkerComment(cursorTestAgentA), recent}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: cursorTestSender,
		},
		{
			name:   "stale marker",
			pages:  [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), cursorTestLabelTime.Add(-20 * time.Minute)}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: cursorTestSender,
		},
		{
			name:   "marker posted after the label",
			pages:  [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), cursorTestLabelTime.Add(10 * time.Minute)}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: cursorTestSender,
		},
		{
			name:   "no check belongs to the agent",
			pages:  [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentB)}},
			sender: cursorTestSender,
		},
		{
			name:   "check made by another app",
			pages:  [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "someone-else", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: cursorTestSender,
		},
		{
			name:   "check name not configured",
			pages:  [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}},
			checks: []cursorFakeCheckRun{{"Cursor Automation: Cherry-pick on merge", "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: cursorTestSender,
		},
		{
			name:   "no marker, a person applied the label",
			pages:  [][]cursorFakeComment{{{cursorTestSender, "looks good to me", recent}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: cursorTestSender,
		},
		{
			name:   "no sender",
			pages:  [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}},
			checks: []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender: "",
		},
		{
			name: "marker on a later page of comments",
			pages: [][]cursorFakeComment{
				{{cursorTestSender, "first page", recent}},
				{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}},
			},
			checks:   []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}},
			sender:   cursorTestSender,
			expected: cursorTestAgentA,
		},
		{
			name: "newest marker wins",
			pages: [][]cursorFakeComment{{
				{cursorTestSender, cursorMarkerComment(cursorTestAgentA), cursorTestLabelTime.Add(-3 * time.Minute)},
				{cursorTestSender, cursorMarkerComment(cursorTestAgentB), cursorTestLabelTime.Add(-5 * time.Second)},
			}},
			checks: []cursorFakeCheckRun{
				{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)},
				{cursorTestExecuteCheck, "cursor", cursorAgentLinkURL(cursorTestAgentB)},
			},
			sender:   cursorTestSender,
			expected: cursorTestAgentB,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newCursorFakeGitHub(t)
			gh.commentPages, gh.checkRuns = tc.pages, tc.checks
			s := cursorTestServer(gh, &MatterwickConfig{})

			agentID, err := s.findRequestingCursorAgent(cursorTestPR(), tc.sender, cursorTestLabelTime, s.Logger)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, agentID)
		})
	}
}

func TestRememberCursorAgentRequest(t *testing.T) {
	recent := cursorTestLabelTime.Add(-4 * time.Second)
	checks := []cursorFakeCheckRun{{cursorTestDesignCheck, "cursor", cursorAgentLinkURL(cursorTestAgentA)}}

	t.Run("stores the agent when an automation asked", func(t *testing.T) {
		gh := newCursorFakeGitHub(t)
		gh.commentPages = [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}}
		gh.checkRuns = checks
		s := cursorTestServer(gh, &MatterwickConfig{})

		s.rememberCursorAgentRequest(cursorTestPR(), cursorTestOwner, cursorTestSender, cursorTestLabelTime)
		assert.Equal(t, cursorTestAgentA, s.takeCursorAgentRequest(cursorTestOwner))
		assert.Empty(t, s.takeCursorAgentRequest(cursorTestOwner), "the entry is read once")
	})

	t.Run("a label applied by a person stores nothing and clears a stale entry", func(t *testing.T) {
		gh := newCursorFakeGitHub(t)
		gh.commentPages = [][]cursorFakeComment{{{cursorTestSender, "no marker here", recent}}}
		gh.checkRuns = checks
		s := cursorTestServer(gh, &MatterwickConfig{})
		s.setCursorAgentRequest(cursorTestOwner, cursorTestAgentB)

		s.rememberCursorAgentRequest(cursorTestPR(), cursorTestOwner, cursorTestSender, cursorTestLabelTime)
		assert.Empty(t, s.takeCursorAgentRequest(cursorTestOwner))
	})

	t.Run("without an API key nothing is looked up", func(t *testing.T) {
		gh := newCursorFakeGitHub(t)
		gh.commentPages = [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}}
		gh.checkRuns = checks
		s := cursorTestServer(gh, &MatterwickConfig{CursorAPIBaseURL: "http://unused.invalid"})
		s.Config.CursorAPIKey = ""

		s.rememberCursorAgentRequest(cursorTestPR(), cursorTestOwner, cursorTestSender, cursorTestLabelTime)
		assert.Empty(t, s.takeCursorAgentRequest(cursorTestOwner))
	})

	t.Run("without configured check names nothing is looked up", func(t *testing.T) {
		gh := newCursorFakeGitHub(t)
		gh.commentPages = [][]cursorFakeComment{{{cursorTestSender, cursorMarkerComment(cursorTestAgentA), recent}}}
		gh.checkRuns = checks
		s := cursorTestServer(gh, &MatterwickConfig{})
		s.Config.CursorAgentCheckNames = nil

		s.rememberCursorAgentRequest(cursorTestPR(), cursorTestOwner, cursorTestSender, cursorTestLabelTime)
		assert.Empty(t, s.takeCursorAgentRequest(cursorTestOwner))
	})
}

type cursorCapture struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  int
	path   string
	user   string
	prompt string
	status int
}

func newCursorCapture(t *testing.T, status int) *cursorCapture {
	t.Helper()
	c := &cursorCapture{status: status}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls++
		c.path = r.URL.Path
		c.user, _, _ = r.BasicAuth()
		var body struct {
			Prompt struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.prompt = body.Prompt.Text
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(`{"run":{"id":"run-1","status":"CREATING"}}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *cursorCapture) snapshot() (calls int, path, user, prompt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.path, c.user, c.prompt
}

func deliveryServer(t *testing.T, gh *cursorFakeGitHub, cursorURL string) (*Server, chan string) {
	t.Helper()
	webhook, messages := newWebhookCaptureServer(t)
	s := cursorTestServer(gh, &MatterwickConfig{
		CursorAPIKey:                    "cursor-key",
		CursorAPIBaseURL:                cursorURL,
		MattermostCredentialsWebhookURL: webhook.URL,
		MattermostCredentialsChannelURL: "https://hub.example.com/channel",
	})
	return s, messages
}

func TestDeliverSpinwickSuccessToCursorAgent(t *testing.T) {
	gh := newCursorFakeGitHub(t)
	cursorSrv := newCursorCapture(t, http.StatusCreated)
	s, messages := deliveryServer(t, gh, cursorSrv.srv.URL)
	s.setCursorAgentRequest(cursorTestOwner, cursorTestAgentA)

	s.deliverSpinwickSuccess(cursorTestPR(), cursorTestInstallation(), "admin-pass", "user-pass", "", cursorTestOwner, s.Logger)

	calls, path, user, prompt := cursorSrv.snapshot()
	assert.Equal(t, 1, calls)
	assert.Equal(t, "/v1/agents/"+cursorTestAgentA+"/runs", path)
	assert.Equal(t, "cursor-key", user)
	assert.Contains(t, prompt, "Status: ready")
	assert.Contains(t, prompt, "**Test Server:** https://pr-7.test.example.com")
	assert.Contains(t, prompt, "| Admin | sysadmin | admin-pass |")
	assert.Contains(t, prompt, "| User | user-1 | user-pass |")
	assert.Contains(t, prompt, "**Installation ID:** `install-id`")

	select {
	case msg := <-messages:
		t.Fatalf("nothing should go to the Mattermost credentials channel, got %q", msg)
	default:
	}

	posted := gh.publicComments()
	require.Len(t, posted, 1)
	assert.Contains(t, posted[0], "Test server created!")
	assert.Contains(t, posted[0], "Sent to the Cursor agent")
	assert.NotContains(t, posted[0], "admin-pass")
	assert.NotContains(t, posted[0], "user-pass")
	assert.NotContains(t, posted[0], "hub.example.com")
}

func TestDeliverSpinwickSuccessFallsBackWhenCursorFails(t *testing.T) {
	gh := newCursorFakeGitHub(t)
	cursorSrv := newCursorCapture(t, http.StatusForbidden)
	s, messages := deliveryServer(t, gh, cursorSrv.srv.URL)
	s.setCursorAgentRequest(cursorTestOwner, cursorTestAgentA)

	s.deliverSpinwickSuccess(cursorTestPR(), cursorTestInstallation(), "admin-pass", "user-pass", "", cursorTestOwner, s.Logger)

	calls, _, _, _ := cursorSrv.snapshot()
	assert.Equal(t, 1, calls)

	msg := <-messages
	assert.Contains(t, msg, "| Admin | sysadmin | admin-pass |")

	posted := gh.publicComments()
	require.Len(t, posted, 1, "the public comment is posted once")
	assert.Contains(t, posted[0], "hub.example.com")
}

// A person applying the label by hand must see exactly what they always have.
func TestDeliverSpinwickSuccessForAPersonIsUnchanged(t *testing.T) {
	gh := newCursorFakeGitHub(t)
	cursorSrv := newCursorCapture(t, http.StatusCreated)
	s, messages := deliveryServer(t, gh, cursorSrv.srv.URL)

	s.deliverSpinwickSuccess(cursorTestPR(), cursorTestInstallation(), "admin-pass", "user-pass", "", cursorTestOwner, s.Logger)

	calls, _, _, _ := cursorSrv.snapshot()
	assert.Equal(t, 0, calls, "Cursor is never contacted")

	msg := <-messages
	assert.Contains(t, msg, "## Mattermost Spinwick for PR #7")
	assert.Contains(t, msg, "| Admin | sysadmin | admin-pass |")

	posted := gh.publicComments()
	require.Len(t, posted, 1)
	assert.Contains(t, posted[0], "Posted securely in [this Mattermost channel](https://hub.example.com/channel)")
}

func TestDeliverSpinwickFailure(t *testing.T) {
	request := (&spinwick.Request{InstallationID: "n/a"}).WithError(errors.New("the installation creation failed"))

	t.Run("an agent is told", func(t *testing.T) {
		gh := newCursorFakeGitHub(t)
		cursorSrv := newCursorCapture(t, http.StatusCreated)
		s, _ := deliveryServer(t, gh, cursorSrv.srv.URL)
		s.setCursorAgentRequest(cursorTestOwner, cursorTestAgentA)

		s.deliverSpinwickFailure(cursorTestPR(), request, cursorTestOwner, s.Logger)

		calls, path, _, prompt := cursorSrv.snapshot()
		assert.Equal(t, 1, calls)
		assert.Equal(t, "/v1/agents/"+cursorTestAgentA+"/runs", path)
		assert.Contains(t, prompt, "Status: failed")
		assert.Contains(t, prompt, "the installation creation failed")
		assert.NotContains(t, prompt, "sysadmin")
		assert.NotContains(t, prompt, "n/a")
	})

	t.Run("no agent, nothing is sent", func(t *testing.T) {
		gh := newCursorFakeGitHub(t)
		cursorSrv := newCursorCapture(t, http.StatusCreated)
		s, _ := deliveryServer(t, gh, cursorSrv.srv.URL)

		s.deliverSpinwickFailure(cursorTestPR(), request, cursorTestOwner, s.Logger)

		calls, _, _, _ := cursorSrv.snapshot()
		assert.Equal(t, 0, calls)
	})
}

func TestSpinwickFailureMessage(t *testing.T) {
	aborted := (&spinwick.Request{InstallationID: "abc"}).WithError(errors.New("Already found a installation")).IntentionalAbort()

	msg := spinwickFailureMessage(cursorTestPR(), aborted)
	assert.Contains(t, msg, "Status: failed")
	assert.Contains(t, msg, "PR #7 (mattermost/mattermost)")
	assert.Contains(t, msg, "Already found a installation")
	assert.Contains(t, msg, "aborted")
	assert.Contains(t, msg, "`abc`")
}

func TestPollInstallationDeleted(t *testing.T) {
	ctx := context.Background()

	t.Run("returns when the installation is deleted", func(t *testing.T) {
		states := []string{cloudModel.InstallationStateDeletionInProgress, cloudModel.InstallationStateDeletionInProgress, cloudModel.InstallationStateDeleted}
		calls := 0
		err := pollInstallationDeleted(ctx, time.Millisecond, func() (string, error) {
			s := states[calls]
			calls++
			return s, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
	})

	t.Run("fails when the deletion fails", func(t *testing.T) {
		err := pollInstallationDeleted(ctx, time.Millisecond, func() (string, error) {
			return cloudModel.InstallationStateDeletionFailed, nil
		})
		require.Error(t, err)
	})

	t.Run("fails when the lookup fails", func(t *testing.T) {
		err := pollInstallationDeleted(ctx, time.Millisecond, func() (string, error) {
			return "", errors.New("provisioner down")
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "provisioner down")
	})

	t.Run("gives up when the time runs out", func(t *testing.T) {
		timeoutCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		err := pollInstallationDeleted(timeoutCtx, time.Millisecond, func() (string, error) {
			return cloudModel.InstallationStateDeletionInProgress, nil
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
	})
}

// The refactor of the success message must leave the Mattermost credentials post as it was.
func TestSpinwickCredentialsMessageKeepsFooterForTheChannel(t *testing.T) {
	s := &Server{Config: &MatterwickConfig{MattermostWebhookFooter: "footer text"}, Logger: logrus.New()}

	withFooter := s.spinwickCredentialsMessage(cursorTestPR(), cursorTestInstallation(), "https://x", "a", "u", "", true)
	withoutFooter := s.spinwickCredentialsMessage(cursorTestPR(), cursorTestInstallation(), "https://x", "a", "u", "", false)

	assert.Contains(t, withFooter, "\n---\nfooter text")
	assert.NotContains(t, withoutFooter, "footer text")
}

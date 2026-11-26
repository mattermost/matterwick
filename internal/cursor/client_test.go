// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package cursor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAgentID = "bc-3450c942-e7b6-4542-808e-d625454cd4c6"

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key")
	c.busyRetries = 2
	c.busyRetryWait = time.Millisecond
	return c
}

func TestIsAgentID(t *testing.T) {
	assert.True(t, IsAgentID(testAgentID))
	assert.False(t, IsAgentID(""))
	assert.False(t, IsAgentID("bc-123"))
	assert.False(t, IsAgentID("../etc/passwd"))
	assert.False(t, IsAgentID(testAgentID+"/extra"))
}

func TestSendFollowUpRequestShape(t *testing.T) {
	var gotMethod, gotPath, gotContentType, gotUser, gotPass string
	var gotBody followUpRequest

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotContentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		gotUser, gotPass, _ = r.BasicAuth()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"run-1","agentId":"` + testAgentID + `","status":"CREATING"}}`))
	})

	followUp, err := c.SendFollowUp(context.Background(), testAgentID, "hello\nworld")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/agents/"+testAgentID+"/runs", gotPath)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "test-key", gotUser)
	assert.Equal(t, "", gotPass)
	assert.Equal(t, "hello\nworld", gotBody.Prompt.Text)
	assert.Equal(t, "run-1", followUp.RunID)
	assert.Equal(t, "CREATING", followUp.Status)
}

func TestSendFollowUpAcceptsAccepted(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`not json`))
	})

	followUp, err := c.SendFollowUp(context.Background(), testAgentID, "x")
	require.NoError(t, err)
	assert.Empty(t, followUp.RunID)
}

func TestSendFollowUpRetriesWhenBusy(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"agent_busy","message":"busy"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"run-2","status":"CREATING"}}`))
	})

	followUp, err := c.SendFollowUp(context.Background(), testAgentID, "x")
	require.NoError(t, err)
	assert.Equal(t, "run-2", followUp.RunID)
	assert.EqualValues(t, 2, atomic.LoadInt32(&calls))
}

func TestSendFollowUpGivesUpWhenStayingBusy(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"agent_busy","message":"busy"}`))
	})

	_, err := c.SendFollowUp(context.Background(), testAgentID, "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_busy")
	assert.EqualValues(t, 3, atomic.LoadInt32(&calls), "one attempt plus two retries")
}

func TestSendFollowUpErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"bad key"}`, "bad key"},
		{"key not supported", http.StatusForbidden, `{"code":"key_not_supported","message":"no"}`, "key_not_supported"},
		{"not found", http.StatusNotFound, `{"error":"no such agent"}`, "no such agent"},
		{"plain text", http.StatusBadGateway, `upstream down`, "upstream down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			_, err := c.SendFollowUp(context.Background(), testAgentID, "x")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "test-key", "the API key must never appear in an error")
			assert.EqualValues(t, 1, atomic.LoadInt32(&calls), "only 409 is retried")
		})
	}
}

func TestSendFollowUpRejectsBadInput(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})

	_, err := c.SendFollowUp(context.Background(), "../bad", "x")
	require.Error(t, err)

	empty := NewClient(c.baseURL, "")
	_, err = empty.SendFollowUp(context.Background(), testAgentID, "x")
	require.Error(t, err)

	assert.EqualValues(t, 0, atomic.LoadInt32(&calls))
}

func TestSendFollowUpStopsWhenContextEnds(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	c.busyRetryWait = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.SendFollowUp(ctx, testAgentID, "x")
	require.Error(t, err)
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

// Package cursor is a small client for the part of the Cursor Cloud Agents API that Matterwick uses:
// sending a follow-up message to an existing cloud agent.
package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	// DefaultBaseURL is the production Cursor API host.
	DefaultBaseURL = "https://api.cursor.com"

	defaultHTTPTimeout   = 30 * time.Second
	defaultBusyRetries   = 5
	defaultBusyRetryWait = 10 * time.Second
	maxErrorBodyBytes    = 512
)

// agentIDPattern matches Cursor cloud agent IDs, for example bc-3450c942-e7b6-4542-808e-d625454cd4c6.
var agentIDPattern = regexp.MustCompile(`^bc-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsAgentID reports whether id has the shape of a Cursor cloud agent ID.
func IsAgentID(id string) bool {
	return agentIDPattern.MatchString(id)
}

// APIError is returned when the Cursor API answers with an unexpected status.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("cursor API returned status %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("cursor API returned status %d: %s", e.StatusCode, e.Message)
}

// FollowUp describes the run that Cursor created for a follow-up message.
type FollowUp struct {
	RunID  string
	Status string
}

// Client sends follow-up messages to Cursor cloud agents.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string

	busyRetries   int
	busyRetryWait time.Duration
}

// NewClient returns a Client for baseURL authenticating with apiKey. An empty baseURL selects DefaultBaseURL.
func NewClient(baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		httpClient:    &http.Client{Timeout: defaultHTTPTimeout},
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		busyRetries:   defaultBusyRetries,
		busyRetryWait: defaultBusyRetryWait,
	}
}

type followUpRequest struct {
	Prompt followUpPrompt `json:"prompt"`
}

type followUpPrompt struct {
	Text string `json:"text"`
}

type followUpResponse struct {
	Run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"run"`
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// SendFollowUp posts text to the agent as a new follow-up prompt. If the agent is still running a turn (409
// agent_busy) the call is retried a few times before giving up. The API key is never included in errors.
func (c *Client) SendFollowUp(ctx context.Context, agentID, text string) (*FollowUp, error) {
	if !IsAgentID(agentID) {
		return nil, errors.Errorf("%q is not a Cursor agent ID", agentID)
	}
	if c.apiKey == "" {
		return nil, errors.New("no Cursor API key configured")
	}

	body, err := json.Marshal(followUpRequest{Prompt: followUpPrompt{Text: text}})
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode the follow-up request")
	}

	endpoint := fmt.Sprintf("%s/v1/agents/%s/runs", c.baseURL, url.PathEscape(agentID))

	var lastErr error
	for attempt := 0; attempt <= c.busyRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, errors.Wrap(ctx.Err(), "gave up waiting for the agent to become idle")
			case <-time.After(c.busyRetryWait):
			}
		}

		followUp, err := c.post(ctx, endpoint, body)
		if err == nil {
			return followUp, nil
		}
		lastErr = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
			return nil, err
		}
	}

	return nil, errors.Wrap(lastErr, "the agent stayed busy")
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte) (*FollowUp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create the follow-up request")
	}
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send the follow-up request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return nil, readAPIError(resp)
	}

	var parsed followUpResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		// The follow-up was accepted. A response we cannot read is not a failure to deliver it.
		return &FollowUp{}, nil
	}
	return &FollowUp{RunID: parsed.Run.ID, Status: parsed.Run.Status}, nil
}

func readAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	apiErr := &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(raw))}

	var parsed errorResponse
	if json.Unmarshal(raw, &parsed) == nil {
		apiErr.Code = parsed.Code
		switch {
		case parsed.Message != "":
			apiErr.Message = parsed.Message
		case parsed.Error != "":
			apiErr.Message = parsed.Error
		}
	}
	return apiErr
}

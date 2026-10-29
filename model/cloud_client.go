package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// CloudClient is the provisioner client plus the calls the upstream client cannot bind to a
// context. The upstream client has no request timeout and no context support, so a hung
// request could never be abandoned.
type CloudClient struct {
	*cloudModel.Client

	address    string
	headers    map[string]string
	httpClient *http.Client
}

// cloudRequestTimeout bounds every context-bound request, cancelled or not.
const cloudRequestTimeout = 2 * time.Minute

// NewCloudClient creates a new cloud client with OAuth.
func NewCloudClient(address, clientID, clientSecret, tokenEndpoint, apiKey string) *CloudClient {
	c := &CloudClient{
		address:    strings.TrimRight(address, "/"),
		headers:    map[string]string{},
		httpClient: &http.Client{Timeout: cloudRequestTimeout},
	}

	switch {
	case os.Getenv("MATTERWICK_LOCAL_TESTING") == "true":
		c.Client = cloudModel.NewClient(address)
	case clientID == "" && clientSecret == "" && tokenEndpoint == "":
		c.headers["x-api-key"] = apiKey
		c.Client = cloudModel.NewClientWithHeaders(address, c.headers)
	default:
		c.Client = cloudModel.NewClientWithOAuth(address, nil, clientID, clientSecret, tokenEndpoint)
		// Same credentials as the upstream client; the token source caches and refreshes.
		creds := &clientcredentials.Config{
			ClientID:       clientID,
			ClientSecret:   clientSecret,
			Scopes:         []string{"offline_access api://provisioner/.default"},
			TokenURL:       tokenEndpoint,
			EndpointParams: url.Values{"grant_type": {"client_credentials"}},
		}
		// A token request is bounded too, or a hung token endpoint would hang every create.
		tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second})
		c.httpClient.Transport = &oauth2.Transport{Source: creds.TokenSource(tokenCtx), Base: http.DefaultTransport}
	}
	return c
}

// CreateInstallationContext is CreateInstallation bound to ctx: cancelling ctx aborts the
// in-flight HTTP request. Errors keep the upstream client's "failed with status code N" form.
func (c *CloudClient) CreateInstallationContext(ctx context.Context, request *cloudModel.CreateInstallationRequest) (*cloudModel.InstallationDTO, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.address+"/api/installations", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("failed with status code %d", resp.StatusCode)
	}
	var installation cloudModel.InstallationDTO
	if err := json.NewDecoder(resp.Body).Decode(&installation); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to decode created installation: %w", err)
	}
	return &installation, nil
}

// GetInstallationContext is GetInstallation bound to ctx: cancelling ctx aborts the
// in-flight HTTP request.
func (c *CloudClient) GetInstallationContext(ctx context.Context, installationID string) (*cloudModel.InstallationDTO, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.address+"/api/installation/"+installationID, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed with status code %d", resp.StatusCode)
	}
	var installation cloudModel.InstallationDTO
	if err := json.NewDecoder(resp.Body).Decode(&installation); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to decode installation: %w", err)
	}
	return &installation, nil
}

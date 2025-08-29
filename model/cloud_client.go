package model

import (
	"os"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

// NewCloudClient creates a new cloud client with OAuth.
func NewCloudClient(address, clientID, clientSecret, tokenEndpoint, apiKey string) *cloudModel.Client {
	if os.Getenv("MATTERWICK_LOCAL_TESTING") == "true" {
		return cloudModel.NewClient(address)
	}

	var headers map[string]string
	if apiKey != "" {
		headers = map[string]string{"x-api-key": apiKey}
	}

	if clientID == "" && clientSecret == "" && tokenEndpoint == "" {
		return cloudModel.NewClientWithHeaders(address, headers)
	}

	return cloudModel.NewClientWithOAuth(address, headers, clientID, clientSecret, tokenEndpoint)
}

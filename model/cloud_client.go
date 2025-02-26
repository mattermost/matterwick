package model

import (
	"os"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

// NewCloudClient creates a new cloud client with OAuth.
func NewCloudClient(address, clientID, clientSecret, tokenEndpoint, apiKey string) *cloudModel.Client {
	var headers map[string]string

	if os.Getenv("MATTERWICK_LOCAL_TESTING") == "true" {
		return cloudModel.NewClient(address)
	}

	if clientID == "" && clientSecret == "" && tokenEndpoint == "" {
		headers = map[string]string{
			"x-api-key": apiKey,
		}

		return cloudModel.NewClientWithHeaders(address, headers)
	}
	return cloudModel.NewClientWithOAuth(address, headers, clientID, clientSecret, tokenEndpoint)
}

package model

import (
	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

// NewCloudClientWithOAuth creates a new cloud client with OAuth.
func NewCloudClientWithOAuth(address, clientID, clientSecret, tokenEndpoint string) *cloudModel.Client {
	var headers map[string]string
	return cloudModel.NewClientWithOAuth(address, headers, clientID, clientSecret, tokenEndpoint)
}

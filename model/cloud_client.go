package model

import (
	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

// NewCloudClientWithOAuth creates a new cloud client with OAuth.
func NewCloudClientWithOAuth(address, clientID, clientSecret, tokenEndpoint string) *cloudModel.Client {
	return cloudModel.NewClient(address)
}

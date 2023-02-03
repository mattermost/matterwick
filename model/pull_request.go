// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package model

import (
	"encoding/json"
	"io"
	"time"
)

// PullRequest defines a pr
type PullRequest struct {
	RepoOwner string
	RepoName  string
	FullName  string
	Number    int
	Username  string
	HeadLabel string
	Ref       string
	Sha       string
	Labels    []string
	State     string
	URL       string
	CreatedAt time.Time
}

// ToJSON converts to json
func (o *PullRequest) ToJSON() (string, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

// PullRequestFromJSON convert from json to the struct
func PullRequestFromJSON(data io.Reader) (*PullRequest, error) {
	var pr PullRequest

	if err := json.NewDecoder(data).Decode(&pr); err != nil {
		return nil, err
	}

	return &pr, nil
}

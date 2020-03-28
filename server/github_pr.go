// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"io"

	"github.com/google/go-github/v28/github"
)

func PullRequestEventFromJson(data io.Reader) *github.PullRequestEvent {
	decoder := json.NewDecoder(data)
	var event github.PullRequestEvent
	if err := decoder.Decode(&event); err != nil {
		return nil
	}

	return &event
}

func IssueCommentEventFromJson(data io.Reader) *github.IssueCommentEvent {
	decoder := json.NewDecoder(data)
	var event github.IssueCommentEvent
	if err := decoder.Decode(&event); err != nil {
		return nil
	}

	return &event
}

func PingEventFromJson(data io.Reader) *github.PingEvent {
	decoder := json.NewDecoder(data)
	var event github.PingEvent
	if err := decoder.Decode(&event); err != nil {
		return nil
	}

	return &event
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package store

import (
	"time"

	"github.com/mattermost/matterwick/model"
)

type StoreResult struct {
	Data interface{}
	Err  *model.AppError
}

type StoreChannel chan StoreResult

func Must(sc StoreChannel) interface{} {
	r := <-sc
	if r.Err != nil {
		time.Sleep(time.Second)
		panic(r.Err)
	}

	return r.Data
}

type Store interface {
	PullRequest() PullRequestStore
	Close()
	DropAllTables()
}

type PullRequestStore interface {
	Save(pr *model.PullRequest) StoreChannel
	Get(repoOwner, repoName string, number int) StoreChannel
	List() StoreChannel
	ListOpen() StoreChannel
}

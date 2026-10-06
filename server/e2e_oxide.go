// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/go-github/v32/github"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

// Oxide E2E profile (mattermost/mattermost-mobile-oxide).
//
// The repo name contains "mobile", so every desktop/mobile substring check would
// otherwise treat it as mattermost-mobile: provision mobile-pr-<N>-* servers
// (colliding with mobile PR numbers), dispatch e2e-detox-pr.yml (absent in Oxide),
// and on PR close delete mattermost-mobile's servers for the same PR number.
// Every E2E entry point therefore checks isOxideRepo first. The profile only
// activates when Config.E2EOxideEnabled is true; otherwise Oxide events are ignored.

const (
	oxideRepoName = "mattermost-mobile-oxide"

	// oxideE2EInstanceType is the instance type and DNS prefix (oxide-pr-<N>-<platform>-<uid>).
	// It must not share a prefix with "mobile" or "desktop" so DNS LIKE patterns stay disjoint.
	oxideE2EInstanceType = "oxide"

	// oxideE2EWorkflowFile is dispatched on the PR head ref. Its "name:" must not be in
	// E2ETestWorkflowNames: completion cleanup is keyed for push/CMT flows, and PR servers
	// are reused across label toggles until PR close / E2E/Reset-Servers / max age.
	oxideE2EWorkflowFile = "e2e-matterwick.yml"
)

// oxideE2EPlatforms are the server slots Oxide provisions per PR: three per mobile OS.
// The Oxide workflow spreads its Maestro shards across the three servers of each OS
// (each shard runs as its own user/team, so shards sharing a server do not interfere).
var oxideE2EPlatforms = []string{
	"android-site-1",
	"android-site-2",
	"android-site-3",
	"ios-site-1",
	"ios-site-2",
	"ios-site-3",
}

// oxideE2EWorkflowInputKeys pairs index-for-index with oxideE2EPlatforms.
var oxideE2EWorkflowInputKeys = []string{
	"ANDROID_SITE_1_URL",
	"ANDROID_SITE_2_URL",
	"ANDROID_SITE_3_URL",
	"IOS_SITE_1_URL",
	"IOS_SITE_2_URL",
	"IOS_SITE_3_URL",
}

// isOxideRepo reports whether repoName is the Oxide repo (exact name, case-insensitive).
func isOxideRepo(repoName string) bool {
	return strings.EqualFold(repoName, oxideRepoName)
}

// lockOxidePRProvisioning blocks until this PR's provisioning/reuse section is free and
// returns its unlock func. E2E/Run-Android and E2E/Run-iOS have different in-progress keys
// (so they dispatch independently), but must not both create a server set: the second
// request waits here, then reuses the instances the first one stored. The per-PR mutex is
// kept for the process lifetime (one small entry per Oxide PR that ran E2E).
func (s *Server) lockOxidePRProvisioning(key string) func() {
	m, _ := s.oxidePRProvisionLocks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// buildURLInputs maps each instance's Platform to its workflow input key. It requires
// exactly one instance per platform so dispatch inputs never drift from the topology.
func buildURLInputs(kind string, instances []*E2EInstance, platforms, keys []string) (map[string]string, error) {
	if len(platforms) != len(keys) {
		return nil, fmt.Errorf("%s E2E topology mismatch: %d platforms, %d input keys", kind, len(platforms), len(keys))
	}
	if len(instances) != len(platforms) {
		return nil, fmt.Errorf("%s E2E requires exactly %d instances, got %d", kind, len(platforms), len(instances))
	}
	platformToURL := make(map[string]string, len(instances))
	for _, inst := range instances {
		platformToURL[inst.Platform] = inst.URL
	}
	inputs := make(map[string]string, len(keys))
	for i, platform := range platforms {
		url, ok := platformToURL[platform]
		if !ok {
			return nil, fmt.Errorf("%s E2E missing instance for platform %s", kind, platform)
		}
		inputs[keys[i]] = url
	}
	return inputs, nil
}

// triggerOxideE2EWorkflow dispatches e2e-matterwick.yml in the Oxide repo.
// Inputs must exactly match the workflow's declared workflow_dispatch inputs:
// GitHub rejects undeclared inputs with 422 and allows at most 10.
func (s *Server) triggerOxideE2EWorkflow(ctx context.Context, client *github.Client, pr *model.PullRequest, instances []*E2EInstance, testPlatform string) error {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":         pr.RepoName,
		"pr":           pr.Number,
		"type":         oxideE2EInstanceType,
		"testPlatform": testPlatform,
	})

	urlInputs, err := buildURLInputs(oxideE2EInstanceType, instances, oxideE2EPlatforms, oxideE2EWorkflowInputKeys)
	if err != nil {
		return err
	}

	inputs := map[string]interface{}{
		"MOBILE_VERSION": pr.Sha,
		"PLATFORM":       testPlatform, // ios/android/both
		"pr_number":      fmt.Sprintf("%d", pr.Number),
	}
	for key, url := range urlInputs {
		inputs[key] = url
	}

	body := map[string]interface{}{
		"ref":    pr.Ref,
		"inputs": inputs,
	}

	logger.WithField("workflow", oxideE2EWorkflowFile).Debug("Triggering Oxide E2E workflow")

	req, err := client.NewRequest("POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", pr.RepoOwner, pr.RepoName, oxideE2EWorkflowFile), body)
	if err != nil {
		return fmt.Errorf("failed to create workflow dispatch request: %w", err)
	}

	if _, err = client.Do(ctx, req, nil); err != nil {
		return fmt.Errorf("failed to trigger oxide e2e workflow: %w", err)
	}

	logger.Info("Successfully triggered Oxide E2E workflow")
	return nil
}

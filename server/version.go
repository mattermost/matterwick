// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/blang/semver"
)

// resolveMattermostServerVersion returns the highest non-alpha/beta release (stable or RC)
// from mattermost/mattermost, cached for 1 hour. Falls back to the last cached version on error.
func (s *Server) resolveMattermostServerVersion() string {
	cfg := strings.TrimSpace(s.Config.E2EServerVersion)
	if cfg == "" {
		s.Logger.Warn("[resolveMattermostServerVersion] E2EServerVersion is empty in config; defaulting to 'latest'")
		cfg = "latest"
	}
	if cfg != "latest" {
		return cfg
	}

	const cacheTTL = 1 * time.Hour

	// Fast path: return the cached version if still fresh.
	s.e2eVersionCacheLock.Lock()
	if s.e2eVersionCache != "" && time.Since(s.e2eVersionCacheTime) < cacheTTL {
		v := s.e2eVersionCache
		s.e2eVersionCacheLock.Unlock()
		s.Logger.WithField("version", v).Debug("[resolveMattermostServerVersion] Returning cached version")
		return v
	}
	s.e2eVersionCacheLock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := newGithubClient(s.Config.GithubAccessToken)

	// githubAPIBase is only set in tests to point to a mock server.
	if s.githubAPIBase != "" {
		if baseURL, parseErr := url.Parse(s.githubAPIBase); parseErr == nil {
			client.BaseURL = baseURL
		}
	}

	var releases []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}

	req, err := client.NewRequest("GET", "/repos/mattermost/mattermost/releases?per_page=100", nil)
	if err != nil {
		s.Logger.WithError(err).Warn("[resolveMattermostServerVersion] Failed to build request")
		return s.cachedVersionOrMaster()
	}
	if _, err = client.Do(ctx, req, &releases); err != nil {
		s.Logger.WithError(err).Warn("[resolveMattermostServerVersion] Failed to fetch releases")
		return s.cachedVersionOrMaster()
	}

	// Sort by semver descending; GitHub's publish-date order can put backport patches ahead of newer minors.
	// Skip alpha/beta — a future v12.0.0-alpha.1 would otherwise rank above v11.9.0-rc1.
	type candidate struct {
		tag string
		ver semver.Version
	}
	var candidates []candidate
	for _, r := range releases {
		if r.Draft {
			continue
		}
		raw := strings.TrimPrefix(r.TagName, "v")
		v, parseErr := semver.Parse(raw)
		if parseErr != nil {
			continue
		}
		if len(v.Pre) > 0 && (v.Pre[0].VersionStr == "alpha" || v.Pre[0].VersionStr == "beta") {
			continue
		}
		candidates = append(candidates, candidate{tag: raw, ver: v})
	}

	if len(candidates) == 0 {
		s.Logger.Warn("[resolveMattermostServerVersion] No release found")
		return s.cachedVersionOrMaster()
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ver.GT(candidates[j].ver)
	})

	version := candidates[0].tag
	s.Logger.WithField("version", version).Info("[resolveMattermostServerVersion] Resolved latest Mattermost server version")

	s.e2eVersionCacheLock.Lock()
	s.e2eVersionCache = version
	s.e2eVersionCacheTime = time.Now()
	s.e2eVersionCacheLock.Unlock()

	return version
}

// cachedVersionOrMaster returns the current cached version under lock, or "master" if none is cached.
func (s *Server) cachedVersionOrMaster() string {
	s.e2eVersionCacheLock.Lock()
	v := s.e2eVersionCache
	s.e2eVersionCacheLock.Unlock()
	if v != "" {
		s.Logger.WithField("version", v).Warn("[resolveMattermostServerVersion] Using last known version")
		return v
	}
	return "master"
}

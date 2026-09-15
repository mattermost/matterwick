// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/blang/semver"
)

// resolveMattermostServerVersion returns the Mattermost version for PR/main E2E.
// CMT does not use this; it goes through cmtServerVersions().
// Plugin SpinWicks do not use this; they go through pluginSpinwickServerVersion().
// "master" (or any explicit non-latest value) is returned unchanged so Cloud can
// pull mattermostdevelopment/mattermost-enterprise-edition:master.
// "latest" or empty looks up the highest non-alpha/beta GitHub release (stable or RC),
// cached for 1 hour, falling back to the last cached version on error.
func (s *Server) resolveMattermostServerVersion() string {
	cfg := strings.TrimSpace(s.Config.E2EServerVersion)
	if cfg == "" {
		s.Logger.Warn("[resolveMattermostServerVersion] E2EServerVersion is empty in config; defaulting to 'latest'")
		cfg = "latest"
	}
	if cfg != "latest" {
		return cfg
	}
	return s.resolveLatestMattermostRelease()
}

// resolveLatestMattermostRelease looks up the highest non-alpha/beta GitHub
// release (stable or RC), cached for 1 hour. Ignores E2EServerVersion.
func (s *Server) resolveLatestMattermostRelease() string {
	const cacheTTL = 1 * time.Hour

	// Fast path: return the cached version if still fresh.
	s.e2eVersionCacheLock.Lock()
	if s.e2eVersionCache != "" && time.Since(s.e2eVersionCacheTime) < cacheTTL {
		v := s.e2eVersionCache
		s.e2eVersionCacheLock.Unlock()
		s.Logger.WithField("version", v).Debug("[resolveLatestMattermostRelease] Returning cached version")
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

	type releaseEntry struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}
	const perPage = 100
	var releases []releaseEntry
	for page := 1; ; page++ {
		req, err := client.NewRequest("GET", fmt.Sprintf("/repos/mattermost/mattermost/releases?per_page=%d&page=%d", perPage, page), nil)
		if err != nil {
			s.Logger.WithError(err).Warn("[resolveLatestMattermostRelease] Failed to build request")
			return s.cachedVersionOrMaster()
		}
		var pageReleases []releaseEntry
		if _, err = client.Do(ctx, req, &pageReleases); err != nil {
			s.Logger.WithError(err).Warn("[resolveLatestMattermostRelease] Failed to fetch releases")
			return s.cachedVersionOrMaster()
		}
		releases = append(releases, pageReleases...)
		if len(pageReleases) < perPage {
			break
		}
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
		if len(v.Pre) > 0 {
			pre := v.Pre[0].VersionStr
			if strings.HasPrefix(pre, "alpha") || strings.HasPrefix(pre, "beta") {
				continue
			}
		}
		candidates = append(candidates, candidate{tag: raw, ver: v})
	}

	if len(candidates) == 0 {
		s.Logger.Warn("[resolveLatestMattermostRelease] No release found")
		return s.cachedVersionOrMaster()
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ver.GT(candidates[j].ver)
	})

	version := candidates[0].tag
	s.Logger.WithField("version", version).Info("[resolveLatestMattermostRelease] Resolved latest Mattermost server version")

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
		s.Logger.WithField("version", v).Warn("[resolveLatestMattermostRelease] Using last known version")
		return v
	}
	return "master"
}

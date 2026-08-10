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

	"github.com/google/go-github/v32/github"
)

// cmtReleaseSet is classified Mattermost release data: newest patch per stable minor, ESR lines, and current RC.
type cmtReleaseSet struct {
	latestStable map[cmtMinorKey]cmtVersion
	esrMinors    map[cmtMinorKey]bool
	bestRC       cmtVersion
	haveRC       bool
}

func (rs cmtReleaseSet) newestStableMinors() []cmtVersion {
	minors := make([]cmtVersion, 0, len(rs.latestStable))
	for _, v := range rs.latestStable {
		minors = append(minors, v)
	}
	sort.Slice(minors, func(i, j int) bool { return minors[j].less(minors[i]) })
	return minors
}

func (rs cmtReleaseSet) isESRLine(v cmtVersion) bool {
	return rs.esrMinors[cmtMinorKey{v.major, v.minor}]
}

// cmtRCChannelTag is one RC-channel candidate used when collapsing same-patch tags.
type cmtRCChannelTag struct {
	Version     cmtVersion
	PublishedAt time.Time // zero if unknown / unparseable
	SHA         string    // empty until resolved; only needed when bare and -rcN compete
}

type cmtPatchKey struct{ major, minor, patch int }

// pickSamePatchRCChannel chooses the RC-channel winner among tags sharing (major, minor, patch).
func pickSamePatchRCChannel(tags []cmtRCChannelTag) cmtVersion {
	if len(tags) == 0 {
		return cmtVersion{}
	}
	bestByLess := func(in []cmtRCChannelTag) cmtVersion {
		best := in[0]
		for _, t := range in[1:] {
			if best.Version.less(t.Version) {
				best = t
			}
		}
		return best.Version
	}
	if len(tags) == 1 {
		return tags[0].Version
	}

	bareIdx := -1
	var rcTags []cmtRCChannelTag
	for i, t := range tags {
		if t.Version.rc == 0 {
			bareIdx = i
		} else {
			rcTags = append(rcTags, t)
		}
	}
	if bareIdx < 0 || len(rcTags) == 0 {
		return bestByLess(tags)
	}

	bare := tags[bareIdx]
	bestRC := rcTags[0]
	for _, t := range rcTags[1:] {
		if bestRC.Version.less(t.Version) {
			bestRC = t
		}
	}

	if bare.SHA != "" && bestRC.SHA != "" && bare.SHA == bestRC.SHA {
		return bare.Version
	}

	if !bare.PublishedAt.IsZero() && !bestRC.PublishedAt.IsZero() {
		if bare.PublishedAt.After(bestRC.PublishedAt) {
			return bare.Version
		}
		if bestRC.PublishedAt.After(bare.PublishedAt) {
			return bestRC.Version
		}
	}
	return bestRC.Version
}

// resolveMattermostTagCommitSHA resolves a Mattermost release tag to its commit SHA (derefs annotated tags).
func (s *Server) resolveMattermostTagCommitSHA(ctx context.Context, client *github.Client, tag string) (string, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	ref, _, err := client.Git.GetRef(ctx, "mattermost", "mattermost", "tags/v"+raw)
	if err != nil {
		return "", fmt.Errorf("get ref for tag v%s: %w", raw, err)
	}
	obj := ref.GetObject()
	if obj == nil || obj.GetSHA() == "" {
		return "", fmt.Errorf("empty object for tag v%s", raw)
	}
	if obj.GetType() == "commit" {
		return obj.GetSHA(), nil
	}
	if obj.GetType() == "tag" {
		annotated, _, tagErr := client.Git.GetTag(ctx, "mattermost", "mattermost", obj.GetSHA())
		if tagErr != nil {
			return "", fmt.Errorf("deref annotated tag v%s: %w", raw, tagErr)
		}
		if annotated.GetObject() == nil || annotated.GetObject().GetSHA() == "" {
			return "", fmt.Errorf("empty commit for annotated tag v%s", raw)
		}
		return annotated.GetObject().GetSHA(), nil
	}
	return obj.GetSHA(), nil
}

// fetchCMTReleaseSet classifies Mattermost releases into newest stable per minor, ESR lines, and current RC.
// GA: not draft, prerelease==false, not -rcN. RC channel: prerelease==true OR tag is -rcN.
func (s *Server) fetchCMTReleaseSet() (cmtReleaseSet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := newGithubClient(s.Config.GithubAccessToken)
	if s.githubAPIBase != "" {
		if baseURL, parseErr := url.Parse(s.githubAPIBase); parseErr == nil {
			client.BaseURL = baseURL
		}
	}

	type ghRelease struct {
		TagName     string `json:"tag_name"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
	}
	var releases []ghRelease
	const perPage = 100
	for page := 1; ; page++ {
		req, err := client.NewRequest("GET", fmt.Sprintf("/repos/mattermost/mattermost/releases?per_page=%d&page=%d", perPage, page), nil)
		if err != nil {
			return cmtReleaseSet{}, fmt.Errorf("failed to build releases request: %w", err)
		}
		var pageReleases []ghRelease
		if _, err = client.Do(ctx, req, &pageReleases); err != nil {
			return cmtReleaseSet{}, fmt.Errorf("failed to fetch releases: %w", err)
		}
		releases = append(releases, pageReleases...)
		if len(pageReleases) < perPage {
			break
		}
	}

	latestStable := map[cmtMinorKey]cmtVersion{}
	esrMinors := map[cmtMinorKey]bool{}
	rcByPatch := map[cmtPatchKey][]cmtRCChannelTag{}

	for _, r := range releases {
		if r.Draft {
			continue
		}
		v, ok := parseCMTVersion(r.TagName)
		if !ok {
			continue
		}
		key := cmtMinorKey{v.major, v.minor}
		rcChannel := r.Prerelease || v.rc > 0
		if rcChannel {
			var publishedAt time.Time
			if r.PublishedAt != "" {
				if t, parseErr := time.Parse(time.RFC3339, r.PublishedAt); parseErr == nil {
					publishedAt = t
				}
			}
			pk := cmtPatchKey{v.major, v.minor, v.patch}
			rcByPatch[pk] = append(rcByPatch[pk], cmtRCChannelTag{
				Version:     v,
				PublishedAt: publishedAt,
			})
			continue
		}
		if cur, exists := latestStable[key]; !exists || cur.less(v) {
			latestStable[key] = v
		}
		if strings.Contains(strings.ToLower(r.Body), "extended support release") {
			esrMinors[key] = true
		}
	}

	var bestRC cmtVersion
	haveRC := false
	for _, tags := range rcByPatch {
		hasBare, hasRCN := false, false
		for _, t := range tags {
			if t.Version.rc == 0 {
				hasBare = true
			} else {
				hasRCN = true
			}
		}
		if hasBare && hasRCN {
			for i := range tags {
				sha, shaErr := s.resolveMattermostTagCommitSHA(ctx, client, tags[i].Version.raw)
				if shaErr != nil {
					s.Logger.WithError(shaErr).WithField("tag", tags[i].Version.raw).
						Warn("[fetchCMTReleaseSet] Failed to resolve tag SHA; continuing without it")
					continue
				}
				tags[i].SHA = sha
			}
		}
		winner := pickSamePatchRCChannel(tags)
		if winner.raw == "" {
			continue
		}
		if !haveRC || bestRC.less(winner) {
			bestRC = winner
			haveRC = true
		}
	}

	return cmtReleaseSet{
		latestStable: latestStable,
		esrMinors:    esrMinors,
		bestRC:       bestRC,
		haveRC:       haveRC,
	}, nil
}

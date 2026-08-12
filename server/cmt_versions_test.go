// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)


func TestResolveCMTServerVersions(t *testing.T) {
	// A realistic releases payload (newest first): an upcoming RC, recent stable minors,
	// and ESR lines flagged in the body. Includes multiple patches per line, a draft, and
	// an EOL ESR (9.11) that must not enter the matrix.
	releasesBody := `[
		{"tag_name":"v11.8.0-rc3","draft":false,"prerelease":true,"body":"Mattermost Platform Release 11.8.0-rc3"},
		{"tag_name":"v11.8.0-rc2","draft":false,"prerelease":true,"body":"rc"},
		{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.2 contains fixes."},
		{"tag_name":"v11.7.0","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.0"},
		{"tag_name":"v11.6.4","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.6.4"},
		{"tag_name":"v11.6.3","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.6.3"},
		{"tag_name":"v11.5.7","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.5.7"},
		{"tag_name":"v11.99.0","draft":true,"prerelease":false,"body":"draft should be ignored"},
		{"tag_name":"v10.11.19","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 10.11.19 contains security fixes."},
		{"tag_name":"v10.11.17","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 10.11.17"},
		{"tag_name":"v9.11.18","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 9.11.18"}
	]`

	t.Run("auto-derives newest 2 ESRs + latest 3 minors + current RC, latest patch each", func(t *testing.T) {
		srv := mockReleasesServer(t, releasesBody, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		got := s.resolveCMTServerVersions()
		// 10.11.19 + 11.7.2 (newest 2 ESRs) + 11.5.7/11.6.4 (fill latest-3) + 11.8.0-rc3,
		// 9.11.18 EOL ESR dropped, latest patch per line, v-stripped, ascending.
		assert.Equal(t, []string{"10.11.19", "11.5.7", "11.6.4", "11.7.2", "11.8.0-rc3"}, got)
	})

	t.Run("cmtServerVersions uses resolve when CMTServerVersions is empty", func(t *testing.T) {
		srv := mockReleasesServer(t, releasesBody, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"
		s.Config.CMTServerVersions = nil

		assert.Equal(t, []string{"10.11.19", "11.5.7", "11.6.4", "11.7.2", "11.8.0-rc3"}, s.cmtServerVersions("desktop"))
	})

	t.Run("explicit CMTServerVersions override skips resolve", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			_, _ = w.Write([]byte(releasesBody))
		}))
		t.Cleanup(srv.Close)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"
		s.Config.CMTServerVersions = []string{"10.11.22", "11.10.0-rc1"}

		assert.Equal(t, []string{"10.11.22", "11.10.0-rc1"}, s.cmtServerVersions("desktop"))
		assert.Equal(t, []string{"10.11.22", "11.10.0-rc1"}, s.cmtServerVersions("mobile"), "manual override applies to mobile too")
		assert.False(t, called, "manual override must not hit the GitHub API")
	})

	t.Run("API error falls back to defaultCMTServerVersions", func(t *testing.T) {
		srv := mockReleasesServer(t, "boom", http.StatusInternalServerError)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"
		s.Config.CMTServerVersions = nil

		assert.Equal(t, defaultCMTServerVersions, s.resolveCMTServerVersions())
		assert.Equal(t, []string{"10.11.22", "11.7.7"}, defaultCMTServerVersions)
		assert.Equal(t, defaultCMTServerVersions, s.cmtServerVersions("desktop"))
		assert.Equal(t, defaultCMTServerVersions, s.cmtServerVersions("mobile"))
	})

	t.Run("cap prefers trailing ESR over oldest feature minor", func(t *testing.T) {
		// 2 ESRs + 3 distinct latest minors + RC = 6 before cap; drop 11.8 (oldest non-ESR).
		body := `[
			{"tag_name":"v11.11.0-rc1","draft":false,"prerelease":true,"body":"rc"},
			{"tag_name":"v11.10.0","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.10.0"},
			{"tag_name":"v11.9.0","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.9.0"},
			{"tag_name":"v11.8.0","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.8.0"},
			{"tag_name":"v11.7.7","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.7"},
			{"tag_name":"v10.11.22","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 10.11.22"}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		got := s.resolveCMTServerVersions()
		assert.Equal(t, []string{"10.11.22", "11.7.7", "11.9.0", "11.10.0", "11.11.0-rc1"}, got)
		assert.NotContains(t, got, "11.8.0")
		assert.Len(t, got, maxCMTServerVersions)
	})

	t.Run("RC omitted when not newer than latest stable", func(t *testing.T) {
		// Only stable releases here; an old RC for an already-released line must not appear.
		body := `[
			{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.2"},
			{"tag_name":"v11.7.0-rc1","draft":false,"prerelease":true,"body":"rc"},
			{"tag_name":"v11.6.4","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.6.4"},
			{"tag_name":"v11.5.7","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.5.7"}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		got := s.resolveCMTServerVersions()
		assert.Equal(t, []string{"11.5.7", "11.6.4", "11.7.2"}, got, "stale RC must be excluded")
	})

	t.Run("parseCMTVersion handles stable, rc, and v-prefix; rejects junk", func(t *testing.T) {
		v, ok := parseCMTVersion("v11.8.0-rc3")
		assert.True(t, ok)
		assert.Equal(t, "11.8.0-rc3", v.raw)
		assert.Equal(t, 3, v.rc)
		v2, ok2 := parseCMTVersion("10.11.19")
		assert.True(t, ok2)
		assert.Equal(t, 0, v2.rc)
		_, ok3 := parseCMTVersion("v11.7.0-beta.1")
		assert.False(t, ok3, "non-rc prerelease suffixes are not CMT versions")
		_, ok4 := parseCMTVersion("not-a-version")
		assert.False(t, ok4)
		// stable sorts above its rc for the same X.Y.Z
		assert.True(t, v.less(v2) == false)
	})
}

// TestMobileCMTVersionSelection covers the mobile version set: newest ESR, latest
// production (newest non-ESR stable), and the current RC — full suite on latest, smoke on others.
func TestMobileCMTVersionSelection(t *testing.T) {
	releasesBody := `[
		{"tag_name":"v11.8.0-rc3","draft":false,"prerelease":true,"body":"rc"},
		{"tag_name":"v11.8.0-rc2","draft":false,"prerelease":true,"body":"rc"},
		{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.2"},
		{"tag_name":"v11.7.1","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.1"},
		{"tag_name":"v11.6.4","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.6.4"},
		{"tag_name":"v11.5.7","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.5.7"},
		{"tag_name":"v10.11.19","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 10.11.19"}
	]`

	t.Run("picks newest ESR + latest production + current RC", func(t *testing.T) {
		srv := mockReleasesServer(t, releasesBody, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		// 11.7.2 is the newest ESR line, 11.6.4 the newest non-ESR stable, 11.8.0-rc3 the RC.
		// 10.11.19 (older ESR) and 11.5.7 (older stable) are left out.
		assert.Equal(t, []string{"11.6.4", "11.7.2", "11.8.0-rc3"}, s.resolveMobileCMTServerVersions())
	})

	t.Run("mobile selection is used for mobile and not for desktop", func(t *testing.T) {
		srv := mockReleasesServer(t, releasesBody, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"
		s.Config.CMTServerVersions = nil

		mobile := s.cmtServerVersions("mobile")
		desktop := s.cmtServerVersions("desktop")
		assert.Len(t, mobile, maxMobileCMTServerVersions)
		assert.Greater(t, len(desktop), len(mobile), "desktop keeps its wider set")
	})

	t.Run("no RC in flight backfills with the next stable line", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.2"},
			{"tag_name":"v11.6.4","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.6.4"},
			{"tag_name":"v11.5.7","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.5.7"}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		// ESR 11.7.2 + latest production 11.6.4, then 11.5.7 backfills the empty RC slot.
		assert.Equal(t, []string{"11.5.7", "11.6.4", "11.7.2"}, s.resolveMobileCMTServerVersions())
	})

	t.Run("an RC older than the newest stable is not selected", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.6.0-rc1","draft":false,"prerelease":true,"body":"stale rc"},
			{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.2"},
			{"tag_name":"v11.8.1","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.8.1"}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		got := s.resolveMobileCMTServerVersions()
		assert.NotContains(t, got, "11.6.0-rc1", "a stale RC must not take the RC slot")
		assert.Contains(t, got, "11.7.2")
		assert.Contains(t, got, "11.8.1")
	})

	t.Run("same-minor RC keeps its own slot beside stable", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.8.2-rc1","draft":false,"prerelease":true,"body":"rc"},
			{"tag_name":"v11.8.1","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.8.1"},
			{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.7.2"}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		got := s.resolveMobileCMTServerVersions()
		assert.Contains(t, got, "11.8.2-rc1", "newer patch-level RC on the newest stable minor must keep a slot")
		assert.Contains(t, got, "11.8.1")
		assert.Contains(t, got, "11.7.2")
	})

	t.Run("no ESR flagged still fills the budget from stable lines", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.8.1","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.8.1"},
			{"tag_name":"v11.7.2","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.7.2"},
			{"tag_name":"v11.6.4","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.6.4"}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		assert.Equal(t, []string{"11.6.4", "11.7.2", "11.8.1"}, s.resolveMobileCMTServerVersions())
	})

	t.Run("prerelease bare X.Y.Z is RC-channel, not latestStable", func(t *testing.T) {
		// GitHub sometimes marks the final RC as a bare prerelease tag. It must not be
		// treated as GA/stable, and with the same commit as -rc3 it wins as bestRC.
		body := `[
			{"tag_name":"v11.10.0","draft":false,"prerelease":true,"body":"rc final","published_at":"2026-07-01T00:00:00Z"},
			{"tag_name":"v11.10.0-rc3","draft":false,"prerelease":true,"body":"rc","published_at":"2026-06-28T00:00:00Z"},
			{"tag_name":"v11.9.0","draft":false,"prerelease":false,"body":"Mattermost Platform Release 11.9.0"},
			{"tag_name":"v11.8.0","draft":false,"prerelease":false,"body":"Mattermost Platform Extended Support Release 11.8.0"}
		]`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/releases"):
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			case strings.Contains(r.URL.Path, "/git/refs/tags/"):
				// Same SHA for bare and rc3 → pick bare 11.10.0 as bestRC.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ref":"refs/tags/x","object":{"type":"commit","sha":"abc123same"}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		got := s.resolveMobileCMTServerVersions()
		assert.Contains(t, got, "11.10.0", "same-SHA bare prerelease should be bestRC")
		assert.NotContains(t, got, "11.10.0-rc3")
		// Bare prerelease must not also appear as a stable line.
		assert.Equal(t, []string{"11.8.0", "11.9.0", "11.10.0"}, got)
	})

	t.Run("API error falls back to the default set", func(t *testing.T) {
		srv := mockReleasesServer(t, "boom", http.StatusInternalServerError)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		assert.Equal(t, defaultCMTServerVersions, s.resolveMobileCMTServerVersions())
		assert.LessOrEqual(t, len(defaultCMTServerVersions), maxMobileCMTServerVersions)
	})
}

func TestPickSamePatchRCChannel(t *testing.T) {
	bare, _ := parseCMTVersion("11.10.0")
	rc2, _ := parseCMTVersion("11.10.0-rc2")
	rc3, _ := parseCMTVersion("11.10.0-rc3")
	earlier := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	t.Run("same SHA prefers bare", func(t *testing.T) {
		got := pickSamePatchRCChannel([]cmtRCChannelTag{
			{Version: bare, SHA: "same", PublishedAt: later},
			{Version: rc3, SHA: "same", PublishedAt: earlier},
		})
		assert.Equal(t, "11.10.0", got.raw)
	})

	t.Run("different SHA prefers later published_at", func(t *testing.T) {
		got := pickSamePatchRCChannel([]cmtRCChannelTag{
			{Version: bare, SHA: "aaa", PublishedAt: earlier},
			{Version: rc3, SHA: "bbb", PublishedAt: later},
		})
		assert.Equal(t, "11.10.0-rc3", got.raw)
	})

	t.Run("different SHA equal published prefers highest rcN", func(t *testing.T) {
		got := pickSamePatchRCChannel([]cmtRCChannelTag{
			{Version: bare, SHA: "aaa", PublishedAt: later},
			{Version: rc2, SHA: "bbb", PublishedAt: later},
			{Version: rc3, SHA: "ccc", PublishedAt: later},
		})
		assert.Equal(t, "11.10.0-rc3", got.raw)
	})

	t.Run("rc-only picks highest rcN", func(t *testing.T) {
		got := pickSamePatchRCChannel([]cmtRCChannelTag{
			{Version: rc2},
			{Version: rc3},
		})
		assert.Equal(t, "11.10.0-rc3", got.raw)
	})
}

func TestFetchCMTReleaseSetPrereleaseClassification(t *testing.T) {
	t.Run("prerelease bare is not stable; same SHA picks bare as bestRC", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.10.0","draft":false,"prerelease":true,"body":"rc","published_at":"2026-07-01T00:00:00Z"},
			{"tag_name":"v11.10.0-rc3","draft":false,"prerelease":true,"body":"rc","published_at":"2026-06-28T00:00:00Z"},
			{"tag_name":"v11.9.0","draft":false,"prerelease":false,"body":"stable","published_at":"2026-06-01T00:00:00Z"}
		]`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/releases"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			case strings.Contains(r.URL.Path, "/git/refs/tags/"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"deadbeef"}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		rs, err := s.fetchCMTReleaseSet()
		require.NoError(t, err)
		_, inStable := rs.latestStable[cmtMinorKey{11, 10}]
		assert.False(t, inStable, "prerelease bare 11.10.0 must not enter latestStable")
		assert.True(t, rs.haveRC)
		assert.Equal(t, "11.10.0", rs.bestRC.raw)
		assert.Equal(t, "11.9.0", rs.latestStable[cmtMinorKey{11, 9}].raw)
	})

	t.Run("different SHA and later published on rc picks rc", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.10.0","draft":false,"prerelease":true,"body":"rc","published_at":"2026-06-01T00:00:00Z"},
			{"tag_name":"v11.10.0-rc3","draft":false,"prerelease":true,"body":"rc","published_at":"2026-07-01T00:00:00Z"},
			{"tag_name":"v11.9.0","draft":false,"prerelease":false,"body":"stable"}
		]`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/releases"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			case strings.HasSuffix(r.URL.Path, "/git/refs/tags/v11.10.0"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"sha-bare"}}`))
			case strings.HasSuffix(r.URL.Path, "/git/refs/tags/v11.10.0-rc3"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"sha-rc3"}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		rs, err := s.fetchCMTReleaseSet()
		require.NoError(t, err)
		assert.Equal(t, "11.10.0-rc3", rs.bestRC.raw)
	})
}

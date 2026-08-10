// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeMobileCMTInstances(versions []string) []*E2EInstance {
	fullVersion := cmtLatestServerVersion(versions)
	return makeMobileCMTInstancesForVersions(versions, fullVersion)
}

// makeMobileCMTInstancesFullSmoke: fullVersion gets five platforms; each smoke version gets one site-3.
func makeMobileCMTInstancesFullSmoke(fullVersion string, smokeVersions []string) []*E2EInstance {
	versions := append([]string{fullVersion}, smokeVersions...)
	return makeMobileCMTInstancesForVersions(versions, fullVersion)
}

func makeMobileCMTInstancesForVersions(versions []string, fullVersion string) []*E2EInstance {
	fullRaw := strings.TrimPrefix(strings.TrimSpace(fullVersion), "v")
	instances := make([]*E2EInstance, 0, len(mobileE2EPlatforms)+len(versions))
	for versionIndex, version := range versions {
		raw := strings.TrimPrefix(strings.TrimSpace(version), "v")
		if fullRaw != "" && raw == fullRaw {
			for platformIndex, platform := range mobileE2EPlatforms {
				instances = append(instances, &E2EInstance{
					Platform:      platform,
					URL:           fmt.Sprintf("https://v%d-site%d.example.com", versionIndex, platformIndex+1),
					ServerVersion: version,
				})
			}
			continue
		}
		instances = append(instances, &E2EInstance{
			Platform:      "site-3",
			URL:           fmt.Sprintf("https://v%d-smoke.example.com", versionIndex),
			ServerVersion: version,
		})
	}
	return instances
}


func TestDesktopCMTMatrix(t *testing.T) {
	t.Run("parses server_versions input", func(t *testing.T) {
		versions := parseServerVersionsFromString("v11.1.0, v11.2.0, v12.0.0")
		assert.Equal(t, []string{"v11.1.0", "v11.2.0", "v12.0.0"}, versions)
	})

	t.Run("buildDesktopCMTMatrixJSON produces correct schema", func(t *testing.T) {
		versions := []string{"v11.1.0", "v11.2.0"}
		instances := []*E2EInstance{
			{URL: "https://v1.example.com", ServerVersion: "v11.1.0"},
			{URL: "https://v2.example.com", ServerVersion: "v11.2.0"},
		}
		jsonStr, err := buildDesktopCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))

		environments, ok := matrix["environment"].([]interface{})
		require.True(t, ok)
		assert.Len(t, environments, 3, "must have linux, macos, windows")
		runners := map[string]string{}
		for _, env := range environments {
			e := env.(map[string]interface{})
			runners[e["os"].(string)] = e["runner"].(string)
		}
		assert.Equal(t, "ubuntu-latest", runners["linux"])
		assert.Equal(t, "macos-26", runners["macos"])
		assert.Equal(t, "windows-2022", runners["windows"])

		servers, ok := matrix["server"].([]interface{})
		require.True(t, ok)
		require.Len(t, servers, 2)
		s0 := servers[0].(map[string]interface{})
		assert.Equal(t, "v11.1.0", s0["version"])
		assert.Equal(t, "https://v1.example.com", s0["url"])
		// Desktop ignores the `latest` field; cmtServer.Latest is shared with mobile but is
		// never set on the desktop path, and `omitempty` keeps it out of the JSON entirely.
		_, has0 := s0["latest"]
		assert.False(t, has0, "desktop matrix must not carry the `latest` field")
		s1 := servers[1].(map[string]interface{})
		assert.Equal(t, "v11.2.0", s1["version"])
		assert.Equal(t, "https://v2.example.com", s1["url"])
		_, has1 := s1["latest"]
		assert.False(t, has1, "desktop matrix must not carry the `latest` field")
	})

	t.Run("CMT tracking key is keyed by dispatched test run id", func(t *testing.T) {
		repoName := "mattermost-desktop"
		var testRunID int64 = 999
		key := cmtInstanceKey(repoName, testRunID)
		assert.Equal(t, "mattermost-desktop-cmt-999", key)
	})

	t.Run("CMT workflow name detection", func(t *testing.T) {
		isCMT := func(name string) bool {
			return strings.Contains(name, "cmt") || strings.Contains(name, "CMT")
		}
		assert.True(t, isCMT("CMT Provisioner"))
		assert.True(t, isCMT("CMT Mobile"))
		assert.True(t, isCMT("cmt-workflow"))
		// The actual test workflow must NOT match — its completion is handled via
		// isE2ETestWorkflow ("Compatibility Matrix Testing" in E2ETestWorkflowNames)
		assert.False(t, isCMT("Compatibility Matrix Testing"))
		assert.False(t, isCMT("E2E Desktop"))
	})
}


func TestMobileCMTMatrix(t *testing.T) {
	t.Run("buildMobileCMTMatrixJSON produces correct schema", func(t *testing.T) {
		versions := []string{"v11.1.0", "v11.2.0"}
		instances := makeMobileCMTInstances(versions)
		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))

		servers, ok := matrix["server"].([]interface{})
		require.True(t, ok)
		require.Len(t, servers, 2)
		s0 := servers[0].(map[string]interface{})
		assert.Equal(t, "v11.1.0", s0["version"])
		// Older version is smoke: one URL replicated into every site field.
		smokeURL := "https://v0-smoke.example.com"
		assert.Equal(t, smokeURL, s0["android_site_1_url"])
		assert.Equal(t, smokeURL, s0["android_site_2_url"])
		assert.Equal(t, smokeURL, s0["ios_site_1_url"])
		assert.Equal(t, smokeURL, s0["ios_site_2_url"])
		assert.Equal(t, smokeURL, s0["site_3_url"])
		assert.Equal(t, smokeURL, s0["url"])
		// Older version: `latest` is omitted entirely (cmtServer.Latest is false, omitempty).
		_, has0 := s0["latest"]
		assert.False(t, has0, "older mobile entries must not carry the `latest` field")
		s1 := servers[1].(map[string]interface{})
		assert.Equal(t, "v11.2.0", s1["version"])
		assert.Equal(t, "https://v1-site1.example.com", s1["android_site_1_url"])
		assert.Equal(t, "https://v1-site5.example.com", s1["site_3_url"])
		// `url` stays populated (site-1) for release branches cut before mobile's
		// five-server CMT rewrite: those workflows read ${{ matrix.server.url }} and would
		// otherwise test against an empty server URL for a whole release cycle.
		assert.Equal(t, "https://v1-site3.example.com", s1["url"])
		// Highest semver with a full five-platform topology gets `latest: true`. The mobile
		// workflow uses this to decide whether to run the whole suite (latest) or just smoke.
		assert.Equal(t, true, s1["latest"])
	})

	t.Run("smoke-only highest semver does not set latest", func(t *testing.T) {
		// Full-suite version dropped: only smoke survivors remain. Highest semver expands
		// smoke URLs but must not advertise latest:true (workflow would treat it as full suite).
		versions := []string{"11.6.4", "11.7.2"}
		instances := makeMobileCMTInstancesForVersions(versions, "") // both smoke
		require.Len(t, instances, 2)

		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))
		servers := matrix["server"].([]interface{})
		require.Len(t, servers, 2)

		for i, raw := range servers {
			entry := raw.(map[string]interface{})
			_, hasLatest := entry["latest"]
			assert.False(t, hasLatest, "index %d (%q) must not carry latest when topology is smoke-only", i, versions[i])
			smokeURL := fmt.Sprintf("https://v%d-smoke.example.com", i)
			assert.Equal(t, smokeURL, entry["android_site_1_url"])
			assert.Equal(t, smokeURL, entry["site_3_url"])
			assert.Equal(t, smokeURL, entry["url"])
		}
	})

	t.Run("CMT_MATRIX carries explicit five-server topology per version", func(t *testing.T) {
		versions := []string{"v11.1.0", "v11.2.0"}
		instances := makeMobileCMTInstances(versions)
		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		assert.Contains(t, jsonStr, "\"server\"")
		assert.Contains(t, jsonStr, "\"android_site_1_url\"")
		assert.Contains(t, jsonStr, "\"android_site_2_url\"")
		assert.Contains(t, jsonStr, "\"ios_site_1_url\"")
		assert.Contains(t, jsonStr, "\"ios_site_2_url\"")
		assert.Contains(t, jsonStr, "\"site_3_url\"")
	})

	t.Run("mobile CMT 5+1+1 topology expands smoke URLs", func(t *testing.T) {
		full := "11.8.0-rc3"
		smoke := []string{"11.6.4", "11.7.2"}
		versions := append([]string{full}, smoke...)
		instances := makeMobileCMTInstancesFullSmoke(full, smoke)
		require.Len(t, instances, 7)

		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))
		servers := matrix["server"].([]interface{})
		require.Len(t, servers, 3)

		latest := servers[0].(map[string]interface{})
		assert.Equal(t, full, latest["version"])
		assert.Equal(t, true, latest["latest"])
		assert.Equal(t, "https://v0-site1.example.com", latest["android_site_1_url"])
		assert.Equal(t, "https://v0-site5.example.com", latest["site_3_url"])
		assert.NotEqual(t, latest["android_site_1_url"], latest["site_3_url"])

		for i, ver := range smoke {
			entry := servers[i+1].(map[string]interface{})
			assert.Equal(t, ver, entry["version"])
			_, hasLatest := entry["latest"]
			assert.False(t, hasLatest)
			smokeURL := fmt.Sprintf("https://v%d-smoke.example.com", i+1)
			assert.Equal(t, smokeURL, entry["android_site_1_url"])
			assert.Equal(t, smokeURL, entry["android_site_2_url"])
			assert.Equal(t, smokeURL, entry["ios_site_1_url"])
			assert.Equal(t, smokeURL, entry["ios_site_2_url"])
			assert.Equal(t, smokeURL, entry["site_3_url"])
			assert.Equal(t, smokeURL, entry["url"])
		}
	})

	t.Run("mobile CMT rejects partial topologies", func(t *testing.T) {
		versions := []string{"v11.1.0", "v11.2.0"}
		instances := makeMobileCMTInstances(versions) // 1 smoke + 5 full = 6
		_, err := buildMobileCMTMatrixJSON(versions, instances[:5])
		require.Error(t, err)
		assert.Contains(t, err.Error(), "instance count mismatch")
	})

	t.Run("mobile CMT marks the highest-semver entry as latest", func(t *testing.T) {
		// 5-element resolved set (today's typical shape): ESR + 3 minors + current RC.
		// Across the boundary cases that matter: ESR is older despite high patch numbers;
		// RC vs stable for the same X.Y.Z should treat stable as higher; multi-digit RC
		// numbers (rc.10 > rc.2). Locking these in so the workflow's latest gate doesn't
		// silently shift if someone tweaks the comparator.
		cases := []struct {
			name          string
			versions      []string
			wantLatestIdx int
		}{
			{
				name:          "ESR + 3 minors + RC: RC's base is newest so RC is latest",
				versions:      []string{"10.11.19", "11.5.7", "11.6.4", "11.7.2", "11.8.0-rc3"},
				wantLatestIdx: 4,
			},
			{
				name:          "ESR with high patch loses to lower-patch newer minor",
				versions:      []string{"10.11.19", "11.0.0"},
				wantLatestIdx: 1,
			},
			{
				name:          "stable beats same-X.Y.Z RC",
				versions:      []string{"11.7.0-rc3", "11.7.0"},
				wantLatestIdx: 1,
			},
			{
				name:          "rc.10 > rc.2 (no string compare)",
				versions:      []string{"11.8.0-rc2", "11.8.0-rc10"},
				wantLatestIdx: 1,
			},
			{
				name:          "single version is latest",
				versions:      []string{"11.7.2"},
				wantLatestIdx: 0,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				instances := makeMobileCMTInstances(tc.versions)
				jsonStr, err := buildMobileCMTMatrixJSON(tc.versions, instances)
				require.NoError(t, err)
				var matrix map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))
				servers := matrix["server"].([]interface{})
				require.Len(t, servers, len(tc.versions))
				for i, raw := range servers {
					s := raw.(map[string]interface{})
					_, has := s["latest"]
					if i == tc.wantLatestIdx {
						assert.Equal(t, true, s["latest"], "index %d (%q) should be latest", i, tc.versions[i])
					} else {
						assert.False(t, has, "index %d (%q) must not carry latest", i, tc.versions[i])
					}
				}
			})
		}
	})

	t.Run("mobile CMT: all unparseable versions => last entry marked latest", func(t *testing.T) {
		versions := []string{"junk", "also-junk"}
		instances := makeMobileCMTInstances(versions)
		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)
		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))
		servers := matrix["server"].([]interface{})
		_, has0 := servers[0].(map[string]interface{})["latest"]
		assert.False(t, has0)
		assert.Equal(t, true, servers[1].(map[string]interface{})["latest"])
	})
}


func TestCMTVersionNormalization(t *testing.T) {
	t.Run("comma-separated input parsed and v-stripped", func(t *testing.T) {
		// parseServerVersionsFromString splits; the CMT loop then strips v from each.
		raw := "v11.0.1, v11.1.0, 11.2.0"
		parsed := parseServerVersionsFromString(raw)
		require.Len(t, parsed, 3)

		var stripped []string
		for _, v := range parsed {
			stripped = append(stripped, strings.TrimPrefix(strings.TrimSpace(v), "v"))
		}
		assert.Equal(t, []string{"11.0.1", "11.1.0", "11.2.0"}, stripped)
	})

	t.Run("CMT matrix JSON contains stripped versions", func(t *testing.T) {
		// buildDesktopCMTMatrixJSON uses instance.ServerVersion directly.
		// With stripped versions, the matrix has Docker Hub compatible version strings.
		versions := []string{"11.0.1", "11.1.0"} // already stripped
		instances := []*E2EInstance{
			{URL: "https://11-0-1.example.com", ServerVersion: "11.0.1"},
			{URL: "https://11-1-0.example.com", ServerVersion: "11.1.0"},
		}
		jsonStr, err := buildDesktopCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))

		servers, ok := matrix["server"].([]interface{})
		require.True(t, ok)
		require.Len(t, servers, 2)

		for _, srv := range servers {
			s := srv.(map[string]interface{})
			ver := s["version"].(string)
			assert.False(t, strings.HasPrefix(ver, "v"),
				"CMT matrix version %q must not have 'v' prefix", ver)
		}

		s0 := servers[0].(map[string]interface{})
		s1 := servers[1].(map[string]interface{})
		assert.Equal(t, "11.0.1", s0["version"])
		assert.Equal(t, "11.1.0", s1["version"])
	})

}

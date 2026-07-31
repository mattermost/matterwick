// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

// Dry-run tests for E2E orchestration logic (no cloud API calls).
// These tests mock the GitHub Actions dispatch endpoint and verify the
// correct workflow is invoked with the correct inputs for every scenario:
//   - Desktop PR label (E2E/Run)
//   - Mobile PR label (E2E/Run, E2E/Run-iOS, E2E/Run-Android)
//   - Desktop push event (release branch, master)
//   - Mobile push event (release branch, master)
//   - Desktop CMT (workflow_run requested)
//   - Mobile CMT (workflow_run requested, one dispatch per version)
//   - Cleanup on label removal
//   - Cleanup on CMT workflow completed

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v32/github"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

// dispatchCapture records a single workflow_dispatch call.
type dispatchCapture struct {
	Workflow string // e.g. "e2e-functional.yml"
	Repo     string
	Ref      string
	Inputs   map[string]interface{}
}

// mockGitHubServer builds a test server that captures workflow dispatch calls
// and returns the given status code for all dispatch requests.
func mockGitHubServer(t *testing.T, status int) (*httptest.Server, *[]dispatchCapture) {
	t.Helper()
	var mu sync.Mutex
	var captures []dispatchCapture

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/dispatches") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var payload struct {
			Ref    string                 `json:"ref"`
			Inputs map[string]interface{} `json:"inputs"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))

		// Extract workflow filename from path: .../workflows/<file>/dispatches
		parts := strings.Split(r.URL.Path, "/")
		workflow := ""
		for i, p := range parts {
			if p == "workflows" && i+1 < len(parts) {
				workflow = parts[i+1]
				break
			}
		}

		// Extract repo from path: /repos/<owner>/<repo>/...
		repo := ""
		for i, p := range parts {
			if p == "repos" && i+2 < len(parts) {
				repo = parts[i+2]
				break
			}
		}

		mu.Lock()
		captures = append(captures, dispatchCapture{
			Workflow: workflow,
			Repo:     repo,
			Ref:      payload.Ref,
			Inputs:   payload.Inputs,
		})
		mu.Unlock()

		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	return srv, &captures
}

// newDryRunServer builds a minimal Server with no cloud client.
func newDryRunServer(t *testing.T, apiBase, org string) *Server {
	t.Helper()
	return &Server{
		Config: &MatterwickConfig{
			GithubAccessToken:       "test-token",
			Org:                     org,
			DNSNameTestServer:       "test.example.com",
			E2ELabel:                "E2E/Run",
			E2EMobileIOSLabel:       "E2E/Run-iOS",
			E2EMobileAndroidLabel:   "E2E/Run-Android",
			E2EUsername:             "e2eadmin",
			E2EPassword:             "e2epassword",
			E2EServerVersion:        "master",
			E2EReleasePatternPrefix: "release-",
		},
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
	}
}

// newTestGitHubClient creates a *github.Client whose BaseURL points at srv so that
// production dispatch functions (triggerDesktopE2EWorkflow, triggerMobileE2EWorkflow, …)
// send their HTTP requests to the mock server instead of api.github.com.
func newTestGitHubClient(t *testing.T, srv *httptest.Server) *gogithub.Client {
	t.Helper()
	client := gogithub.NewClient(nil)
	baseURL, err := url.Parse(srv.URL + "/")
	require.NoError(t, err)
	client.BaseURL = baseURL
	return client
}

// makeDesktopInstances fabricates the 3 desktop instances (linux/macos/windows).
func makeDesktopInstances() []*E2EInstance {
	return []*E2EInstance{
		{Name: "inst-linux", Platform: "linux", Runner: "ubuntu-latest", URL: "https://linux.test.example.com", InstallationID: "id-1", ServerVersion: "master"},
		{Name: "inst-macos", Platform: "macos", Runner: "macos-latest", URL: "https://macos.test.example.com", InstallationID: "id-2", ServerVersion: "master"},
		{Name: "inst-windows", Platform: "windows", Runner: "windows-2022", URL: "https://windows.test.example.com", InstallationID: "id-3", ServerVersion: "master"},
	}
}

// makeMobileInstances fabricates the 5 platform-isolated mobile instances.
func makeMobileInstances() []*E2EInstance {
	return []*E2EInstance{
		{Name: "inst-android-site1", Platform: "android-site-1", URL: "https://android-site1.test.example.com", InstallationID: "id-1", ServerVersion: "master"},
		{Name: "inst-android-site2", Platform: "android-site-2", URL: "https://android-site2.test.example.com", InstallationID: "id-2", ServerVersion: "master"},
		{Name: "inst-ios-site1", Platform: "ios-site-1", URL: "https://ios-site1.test.example.com", InstallationID: "id-3", ServerVersion: "master"},
		{Name: "inst-ios-site2", Platform: "ios-site-2", URL: "https://ios-site2.test.example.com", InstallationID: "id-4", ServerVersion: "master"},
		{Name: "inst-site3", Platform: "site-3", URL: "https://site3.test.example.com", InstallationID: "id-5", ServerVersion: "master"},
	}
}

func makeMobileCMTInstances(versions []string) []*E2EInstance {
	instances := make([]*E2EInstance, 0, len(versions)*len(mobileE2EPlatforms))
	for versionIndex, version := range versions {
		for platformIndex, platform := range mobileE2EPlatforms {
			instances = append(instances, &E2EInstance{
				Platform:      platform,
				URL:           fmt.Sprintf("https://v%d-site%d.example.com", versionIndex, platformIndex+1),
				ServerVersion: version,
			})
		}
	}
	return instances
}

// ------------------------------------------------------------
// 1. Desktop PR label: E2E/Run → e2e-functional.yml
// ------------------------------------------------------------

func TestDryRun_DesktopDispatch(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")
	instances := makeDesktopInstances()

	instanceDetailsJSON, err := s.buildInstanceDetailsJSON(instances)
	require.NoError(t, err)

	t.Run("instance_details JSON schema", func(t *testing.T) {
		var details []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(instanceDetailsJSON), &details))
		require.Len(t, details, 3)

		platformOrder := []string{"linux", "macos", "windows"}
		runnerMap := map[string]string{
			"linux":   "ubuntu-latest",
			"macos":   "macos-latest",
			"windows": "windows-2022",
		}

		for i, d := range details {
			assert.Equal(t, platformOrder[i], d["platform"], "platform mismatch at index %d", i)
			assert.Equal(t, runnerMap[platformOrder[i]], d["runner"], "runner mismatch at index %d", i)
			assert.NotEmpty(t, d["url"], "url missing at index %d", i)
			assert.NotEmpty(t, d["installation-id"], "installation-id missing at index %d", i)
			assert.NotEmpty(t, d["server_version"], "server_version missing at index %d", i)
		}
	})

	t.Run("workflow inputs built correctly", func(t *testing.T) {
		// Drive the real triggerDesktopE2EWorkflow so assertions validate the
		// actual payload produced by production code, not a hand-built map.
		ghSrv, captures := mockGitHubServer(t, http.StatusNoContent)
		client := newTestGitHubClient(t, ghSrv)
		pr := &model.PullRequest{
			RepoOwner: "mattermost",
			RepoName:  "mattermost-desktop",
			Number:    42,
			Ref:       "feature-branch",
			Sha:       "abc123",
		}

		err := s.triggerDesktopE2EWorkflow(context.Background(), client, pr, instances)
		require.NoError(t, err)
		require.Len(t, *captures, 1)

		c := (*captures)[0]
		// MM_SERVER_VERSION must come from instances[0].ServerVersion, NOT from
		// s.Config.E2EServerVersion (which may be "latest").
		assert.Equal(t, instances[0].ServerVersion, c.Inputs["MM_SERVER_VERSION"],
			"MM_SERVER_VERSION must come from instances[0].ServerVersion, not from config")
		assert.Equal(t, s.Config.E2EUsername, c.Inputs["MM_TEST_USER_NAME"])
		assert.Equal(t, pr.Ref, c.Inputs["version_name"])
		assert.NotEmpty(t, c.Inputs["instance_details"])
	})

}

// ------------------------------------------------------------
// 2. Mobile PR label: three label variants → correct PLATFORM
// ------------------------------------------------------------

func TestDryRun_MobileDispatch(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")
	instances := makeMobileInstances()
	prSha := "def456"
	prRef := "feature-mobile"

	for _, tt := range []struct {
		label    string
		platform string
	}{
		{"E2E/Run", "both"},
		{"E2E/Run-iOS", "ios"},
		{"E2E/Run-Android", "android"},
	} {
		t.Run("label "+tt.label+" → PLATFORM="+tt.platform, func(t *testing.T) {
			platform := s.extractPlatformFromLabel(tt.label)
			assert.Equal(t, tt.platform, platform)

			ghSrv, captures := mockGitHubServer(t, http.StatusNoContent)
			client := newTestGitHubClient(t, ghSrv)
			pr := &model.PullRequest{
				RepoOwner: "mattermost",
				RepoName:  "mattermost-mobile",
				Number:    42,
				Ref:       prRef,
				Sha:       prSha,
			}
			require.NoError(t, s.triggerMobileE2EWorkflow(context.Background(), client, pr, instances, platform))
			require.Len(t, *captures, 1)

			capture := (*captures)[0]
			assert.Equal(t, prRef, capture.Ref)
			assert.Equal(t, tt.platform, capture.Inputs["PLATFORM"])
			assert.Equal(t, prSha, capture.Inputs["MOBILE_VERSION"])
			assert.Equal(t, "https://android-site1.test.example.com", capture.Inputs["ANDROID_SITE_1_URL"])
			assert.Equal(t, "https://android-site2.test.example.com", capture.Inputs["ANDROID_SITE_2_URL"])
			assert.Equal(t, "https://ios-site1.test.example.com", capture.Inputs["IOS_SITE_1_URL"])
			assert.Equal(t, "https://ios-site2.test.example.com", capture.Inputs["IOS_SITE_2_URL"])
			assert.Equal(t, "https://site3.test.example.com", capture.Inputs["SITE_3_URL"])

			// Mobile must NOT use instance_details (desktop-only field)
			assert.NotContains(t, capture.Inputs, "instance_details",
				"mobile workflow must not send instance_details")
			assert.NotContains(t, capture.Inputs, "SITE_1_URL",
				"new Matterwick dispatches must use explicit platform URL inputs")
			assert.NotContains(t, capture.Inputs, "SITE_2_URL",
				"new Matterwick dispatches must use explicit platform URL inputs")
		})
	}

}

func TestDryRun_MobileTopology(t *testing.T) {
	assert.Equal(t, []string{
		"android-site-1",
		"android-site-2",
		"ios-site-1",
		"ios-site-2",
		"site-3",
	}, mobileE2EPlatforms)
	assert.Equal(t, []string{
		"ANDROID_SITE_1_URL",
		"ANDROID_SITE_2_URL",
		"IOS_SITE_1_URL",
		"IOS_SITE_2_URL",
		"SITE_3_URL",
	}, mobileE2EWorkflowInputKeys)

	s := newDryRunServer(t, "", "mattermost")
	err := s.triggerMobileE2EWorkflowForPushEvent(
		"mattermost",
		"mattermost-mobile",
		"main",
		"abc123",
		makeMobileInstances()[:3],
	)
	require.EqualError(t, err, "mobile E2E requires 5 instances")
}

// ------------------------------------------------------------
// 3. Label detection for all configured E2E labels
// ------------------------------------------------------------

func TestDryRun_LabelDetection(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")

	tests := []struct {
		label    string
		isE2E    bool
		platform string
	}{
		{"E2E/Run", true, "both"},
		{"E2E/Run-iOS", true, "ios"},
		{"E2E/Run-Android", true, "android"},
		{"spinwick", false, ""},
		{"E2E/Run-Desktop", false, ""}, // not configured
		{"", false, ""},
		{"e2e/run", false, ""}, // case-sensitive
	}

	for _, tt := range tests {
		t.Run(tt.label+"_isE2E", func(t *testing.T) {
			assert.Equal(t, tt.isE2E, s.isE2ELabel(tt.label))
		})
		if tt.isE2E {
			t.Run(tt.label+"_platform", func(t *testing.T) {
				assert.Equal(t, tt.platform, s.extractPlatformFromLabel(tt.label))
			})
		}
	}
}

// ------------------------------------------------------------
// 5. Desktop push event logic
// ------------------------------------------------------------

func TestDryRun_DesktopPushEvent(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")

	t.Run("release branch detection", func(t *testing.T) {
		assert.True(t, s.isReleaseBranch("release-8.0"))
		assert.True(t, s.isReleaseBranch("release-10.5"))
		assert.False(t, s.isReleaseBranch("master"))
		assert.False(t, s.isReleaseBranch("main"))
		assert.False(t, s.isReleaseBranch("feature-branch"))
	})

	t.Run("empty release pattern prefix never matches any branch", func(t *testing.T) {
		// strings.HasPrefix(any, "") is always true. Without this guard, a missing
		// or empty E2EReleasePatternPrefix in the deployed config would classify
		// every push (master, feature branches, anything) as a release branch and
		// trigger spurious E2E provisioning on every push.
		empty := newDryRunServer(t, "", "mattermost")
		empty.Config.E2EReleasePatternPrefix = ""

		assert.False(t, empty.isReleaseBranch("master"),
			"empty prefix must not match master")
		assert.False(t, empty.isReleaseBranch("release-8.0"),
			"empty prefix must not match release-8.0")
		assert.False(t, empty.isReleaseBranch("anything"),
			"empty prefix must not match arbitrary branches")
		assert.False(t, empty.isReleaseBranch(""),
			"empty prefix must not match empty branch")
	})

	t.Run("branch name extracted from git ref", func(t *testing.T) {
		assert.Equal(t, "release-8.0", extractBranchName("refs/heads/release-8.0"))
		assert.Equal(t, "master", extractBranchName("refs/heads/master"))
		assert.Equal(t, "feature/my-branch", extractBranchName("refs/heads/feature/my-branch"))
	})

	t.Run("tag refs are not treated as branch refs", func(t *testing.T) {
		// extractBranchName("refs/tags/release-9.0") returns "release-9.0",
		// which would match isReleaseBranch and trigger unintended E2E provisioning.
		// handlePushEvent must guard against non-refs/heads/ refs before calling extractBranchName.
		tagRef := "refs/tags/release-9.0"
		assert.False(t, strings.HasPrefix(tagRef, "refs/heads/"),
			"tag ref must be filtered before branch-trigger evaluation")
		// The extracted value looks like a release branch — the guard is what prevents it.
		assert.Equal(t, "release-9.0", extractBranchName(tagRef),
			"extractBranchName is unaware of ref type; caller must pre-filter")
	})

	t.Run("desktop push instance_details carries server_version", func(t *testing.T) {
		instances := makeDesktopInstances()
		instanceDetailsJSON, err := s.buildInstanceDetailsJSON(instances)
		require.NoError(t, err)

		var details []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(instanceDetailsJSON), &details))
		for _, d := range details {
			assert.NotEmpty(t, d["server_version"])
		}
	})
}

// ------------------------------------------------------------
// 7. Desktop CMT logic
// ------------------------------------------------------------

func TestDryRun_DesktopCMT(t *testing.T) {
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

// ------------------------------------------------------------
// 8. Mobile CMT logic
// ------------------------------------------------------------

func TestDryRun_MobileCMT(t *testing.T) {
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
		assert.Equal(t, "https://v0-site1.example.com", s0["android_site_1_url"])
		assert.Equal(t, "https://v0-site2.example.com", s0["android_site_2_url"])
		assert.Equal(t, "https://v0-site3.example.com", s0["ios_site_1_url"])
		assert.Equal(t, "https://v0-site4.example.com", s0["ios_site_2_url"])
		assert.Equal(t, "https://v0-site5.example.com", s0["site_3_url"])
		// `url` stays populated (site-1) for release branches cut before mobile's
		// five-server CMT rewrite: those workflows read ${{ matrix.server.url }} and would
		// otherwise test against an empty server URL for a whole release cycle.
		assert.Equal(t, "https://v0-site3.example.com", s0["url"])
		// Older version: `latest` is omitted entirely (cmtServer.Latest is false, omitempty).
		_, has0 := s0["latest"]
		assert.False(t, has0, "older mobile entries must not carry the `latest` field")
		s1 := servers[1].(map[string]interface{})
		assert.Equal(t, "v11.2.0", s1["version"])
		assert.Equal(t, "https://v1-site1.example.com", s1["android_site_1_url"])
		assert.Equal(t, "https://v1-site5.example.com", s1["site_3_url"])
		// Highest semver gets `latest: true`. The mobile workflow uses this to decide whether
		// to run the whole suite (latest) or just smoke (older) — that policy lives there,
		// not in matterwick.
		assert.Equal(t, true, s1["latest"])
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

	t.Run("mobile CMT rejects partial topologies", func(t *testing.T) {
		versions := []string{"v11.1.0", "v11.2.0"}
		_, err := buildMobileCMTMatrixJSON(versions, makeMobileCMTInstances(versions)[:9])
		require.EqualError(t, err, "mobile CMT requires 10 instances for 2 versions, got 9")
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

// ------------------------------------------------------------
// 9. Instance tracking and cleanup
// ------------------------------------------------------------

func TestDryRun_InstanceTracking(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")

	t.Run("PR instances stored and retrievable by key", func(t *testing.T) {
		pr := &model.PullRequest{RepoName: "mattermost-desktop", Number: 42}
		key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
		instances := makeDesktopInstances()

		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = instances
		s.e2eInstancesLock.Unlock()

		s.e2eInstancesLock.Lock()
		stored, ok := s.e2eInstances[key]
		s.e2eInstancesLock.Unlock()

		assert.True(t, ok)
		assert.Len(t, stored, 3)
	})

	t.Run("cleanup removes instances from map", func(t *testing.T) {
		pr := &model.PullRequest{RepoName: "mattermost-mobile", Number: 99}
		key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)
		instances := makeMobileInstances()

		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = instances
		s.e2eInstancesLock.Unlock()

		// Simulate handleE2ECleanup
		s.e2eInstancesLock.Lock()
		retrieved := s.e2eInstances[key]
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()

		assert.Len(t, retrieved, 5)

		s.e2eInstancesLock.Lock()
		_, exists := s.e2eInstances[key]
		s.e2eInstancesLock.Unlock()
		assert.False(t, exists)
	})

	t.Run("duplicate PR label cancels old run — old instances removed before new stored", func(t *testing.T) {
		pr := &model.PullRequest{RepoName: "mattermost-desktop", Number: 77}
		key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)

		firstInstances := makeDesktopInstances()
		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = firstInstances
		s.e2eInstancesLock.Unlock()

		// handleE2ETestRequest: detect existing, delete, then later store new
		s.e2eInstancesLock.Lock()
		existing, hasExisting := s.e2eInstances[key]
		if hasExisting {
			delete(s.e2eInstances, key)
		}
		s.e2eInstancesLock.Unlock()

		assert.True(t, hasExisting)
		assert.Len(t, existing, 3)

		// Map should be clean for new run
		s.e2eInstancesLock.Lock()
		_, stillExists := s.e2eInstances[key]
		s.e2eInstancesLock.Unlock()
		assert.False(t, stillExists)
	})

	t.Run("push event tracking key format", func(t *testing.T) {
		key := fmt.Sprintf("%s-push-%s-%s", "mattermost-mobile", "release-8.0", "deadbeef")
		assert.Equal(t, "mattermost-mobile-push-release-8.0-deadbeef", key)
	})

	t.Run("push cleanup collects all SHA-suffixed keys for same branch", func(t *testing.T) {
		repoName := "mattermost-mobile"
		branch := "release-8.0"
		baseKey := fmt.Sprintf("%s-push-%s", repoName, branch)
		key1 := baseKey + "-sha111"
		key2 := baseKey + "-sha222"

		s.e2eInstancesLock.Lock()
		s.e2eInstances[key1] = makeMobileInstances()
		s.e2eInstances[key2] = makeMobileInstances()
		s.e2eInstancesLock.Unlock()

		// Simulate handlePushEventE2ECleanup
		s.e2eInstancesLock.Lock()
		var collected []*E2EInstance
		prefix := baseKey + "-"
		for k, v := range s.e2eInstances {
			if strings.HasPrefix(k, prefix) {
				collected = append(collected, v...)
				delete(s.e2eInstances, k)
			}
		}
		s.e2eInstancesLock.Unlock()

		assert.Len(t, collected, 10, "should collect 5 instances from each of 2 push keys")
	})

	t.Run("CMT cleanup by run id removes only the matching tracking key", func(t *testing.T) {
		repoName := "mattermost-desktop"
		var testRunID int64 = 42
		key := cmtInstanceKey(repoName, testRunID)

		cmtInstances := makeDesktopInstances()
		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = cmtInstances
		s.e2eInstancesLock.Unlock()

		assert.True(t, instanceKeyMatchesRunID(key, repoName, testRunID))

		// Exercise the live map-mutation helper, not a hand-rolled loop — so a future
		// change to removeCMTInstancesByRunID's locking, key derivation, or return shape
		// fails this test instead of silently passing.
		removed := s.removeCMTInstancesByRunID(repoName, testRunID, s.Logger)
		assert.Len(t, removed, 3, "must return the removed instances so the caller can destroy them")

		s.e2eInstancesLock.Lock()
		_, exists := s.e2eInstances[key]
		s.e2eInstancesLock.Unlock()
		assert.False(t, exists, "matching key must be removed from the tracking map")
	})

	t.Run("concurrent CMT runs on same SHA only destroy the completing run", func(t *testing.T) {
		repoName := "mattermost-mobile"
		run1 := int64(100)
		run2 := int64(200)
		key1 := cmtInstanceKey(repoName, run1)
		key2 := cmtInstanceKey(repoName, run2)

		s.e2eInstancesLock.Lock()
		s.e2eInstances[key1] = makeDesktopInstances()
		s.e2eInstances[key2] = makeDesktopInstances()
		s.e2eInstancesLock.Unlock()

		// Use the live map-mutation helper so this regression-tests the real path.
		removed := s.removeCMTInstancesByRunID(repoName, run1, s.Logger)
		assert.Len(t, removed, 3, "run1's instances must be returned")

		s.e2eInstancesLock.Lock()
		_, exists1 := s.e2eInstances[key1]
		_, exists2 := s.e2eInstances[key2]
		s.e2eInstancesLock.Unlock()
		assert.False(t, exists1, "removeCMTInstancesByRunID must remove run1's key")
		assert.True(t, exists2, "other concurrent CMT run must survive cleanup of cancelled run")

		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key2)
		s.e2eInstancesLock.Unlock()
	})

	t.Run("CMT cleanup by run id is a no-op when run id is zero", func(t *testing.T) {
		repoName := "mattermost-mobile"
		key := cmtInstanceKey(repoName, 777)
		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = makeDesktopInstances()
		s.e2eInstancesLock.Unlock()
		defer func() {
			s.e2eInstancesLock.Lock()
			delete(s.e2eInstances, key)
			s.e2eInstancesLock.Unlock()
		}()

		// runID 0 is the "could not resolve" sentinel from pollDispatchedWorkflowRun.
		// The helper must NOT match any key in that case.
		removed := s.removeCMTInstancesByRunID(repoName, 0, s.Logger)
		assert.Nil(t, removed, "zero run id must return nil so no destroy fires")
		s.e2eInstancesLock.Lock()
		_, exists := s.e2eInstances[key]
		s.e2eInstancesLock.Unlock()
		assert.True(t, exists, "zero run id must leave all keys intact")
	})
}

// ------------------------------------------------------------
// 9b. SHA-scoped cleanup must not reap a concurrent flow on the same SHA
// ------------------------------------------------------------

func TestInstanceKeyMatchesRunID(t *testing.T) {
	repo := "mattermost-mobile"
	runID := int64(100)
	cmtKey := cmtInstanceKey(repo, runID)
	otherRunKey := cmtInstanceKey(repo, 200)

	assert.True(t, instanceKeyMatchesRunID(cmtKey, repo, runID))
	assert.False(t, instanceKeyMatchesRunID(otherRunKey, repo, runID))
	assert.False(t, instanceKeyMatchesRunID(cmtKey, "mattermost-desktop", runID))
}

func TestInstanceKeyMatchesSHA(t *testing.T) {
	repo := "mattermost-mobile"
	sha := "deadbeef"
	nightlyKey := fmt.Sprintf("%s-scheduled-200-%s", repo, sha)   // nightly flow, same SHA
	pushKey := fmt.Sprintf("%s-push-release-9.0-%s", repo, sha)   // push flow, same SHA
	prKey := fmt.Sprintf("%s-pr-42", repo)                        // PR flow, no -sha suffix
	legacyCMTKey := fmt.Sprintf("%s-cmt-100-%s", repo, sha)       // legacy CMT key shape (runID + sha) — pre-refactor
	liveCMTKey := cmtInstanceKey(repo, 100)                       // live CMT key shape ({repo}-cmt-{runID}, no sha)
	otherSHAKey := fmt.Sprintf("%s-cmt-100-%s", repo, "feedface") // legacy CMT key with different sha

	t.Run("non-CMT completion matches push/scheduled but NEVER any CMT shape", func(t *testing.T) {
		assert.True(t, instanceKeyMatchesSHA(nightlyKey, repo, sha, false))
		assert.True(t, instanceKeyMatchesSHA(pushKey, repo, sha, false))
		assert.False(t, instanceKeyMatchesSHA(prKey, repo, sha, false), "PR keys have no -sha suffix")
		assert.False(t, instanceKeyMatchesSHA(legacyCMTKey, repo, sha, false), "legacy CMT keys are CMT-prefixed and never SHA-cleaned by non-CMT completions")
		assert.False(t, instanceKeyMatchesSHA(liveCMTKey, repo, sha, false), "live CMT keys have no -sha suffix so cannot match a non-CMT SHA cleanup")
		assert.False(t, instanceKeyMatchesSHA(otherSHAKey, repo, sha, false), "legacy CMT key with different sha must not match")
	})

	t.Run("other repo is never matched", func(t *testing.T) {
		assert.False(t, instanceKeyMatchesSHA("mattermost-desktop-cmt-100-"+sha, repo, sha, true))
	})
}

// ------------------------------------------------------------
// 10. Instance name length safety
// ------------------------------------------------------------

func TestDryRun_InstanceNameLength(t *testing.T) {
	t.Run("long repo name gets truncated to fit DNS limit", func(t *testing.T) {
		dnsDomain := "test.example.com" // len=16
		sanitizedRepo := "mattermost-desktop-enterprise-edition-long-name"
		prNumber := 12345
		platform := "linux"

		suffix := fmt.Sprintf("-e2e-%d-%s", prNumber, platform)
		maxRepoLen := 63 - len(dnsDomain) - len(suffix)
		if maxRepoLen < 1 {
			maxRepoLen = 1
		}

		repo := sanitizedRepo
		if len(repo) > maxRepoLen {
			repo = strings.TrimRight(repo[:maxRepoLen], "-")
		}
		instanceName := repo + suffix

		// The code ensures len(instanceName) + len(DNSNameTestServer) <= 63
		// (conservative limit to keep combined subdomain+domain within 63 chars)
		assert.LessOrEqual(t, len(instanceName)+len(dnsDomain), 63)
	})

}

// ------------------------------------------------------------
// 11. Concurrent safety of e2eInstances map
// ------------------------------------------------------------

func TestDryRun_ConcurrentInstanceAccess(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		key := fmt.Sprintf("mattermost-mobile-pr-%d", i)
		instances := makeMobileInstances()

		go func(k string, insts []*E2EInstance) {
			defer wg.Done()
			s.e2eInstancesLock.Lock()
			s.e2eInstances[k] = insts
			s.e2eInstancesLock.Unlock()
		}(key, instances)

		go func(k string) {
			defer wg.Done()
			s.e2eInstancesLock.Lock()
			delete(s.e2eInstances, k)
			s.e2eInstancesLock.Unlock()
		}(key)
	}

	wg.Wait()
	// No race detected → concurrent access is safe
}

// mockReleasesServer returns an httptest.Server that serves the given body/status
// for any GET request whose path contains "/releases".
func mockReleasesServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newDryRunServerLatest builds a Server with E2EServerVersion="latest" whose
// GitHub API calls are redirected to mockSrv.
func newDryRunServerLatest(t *testing.T, mockSrv *httptest.Server) *Server {
	t.Helper()
	s := newDryRunServer(t, "", "mattermost")
	s.Config.E2EServerVersion = "latest"
	s.githubAPIBase = mockSrv.URL + "/" // must have trailing slash
	return s
}

func TestDryRun_ResolveE2EServerVersion(t *testing.T) {
	t.Run("non-latest config returned unchanged, no API call", func(t *testing.T) {
		// The mock server should never be hit for non-"latest" configs.
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		for _, cfg := range []string{"10.0.0", "master", "9.4.0", "11.6.0"} {
			s := newDryRunServer(t, "", "mattermost")
			s.Config.E2EServerVersion = cfg
			s.githubAPIBase = srv.URL + "/"
			assert.Equal(t, cfg, s.resolveMattermostServerVersion(), "config=%q should be returned unchanged", cfg)
		}
		assert.False(t, called, "GitHub API must not be called when E2EServerVersion is not 'latest'")
	})

	t.Run("empty config falls back to latest resolution, not empty version", func(t *testing.T) {
		// A missing E2EServerVersion field in the deployed config decodes to "".
		// Before the fix, "" was returned as-is and flowed to CreateInstallation,
		// which silently failed to provision any server. Empty must be treated
		// as "latest" so the GitHub-releases lookup runs.
		body := `[{"tag_name":"v12.0.0","draft":false,"prerelease":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.Config.E2EServerVersion = ""
		s.githubAPIBase = srv.URL + "/"

		assert.Equal(t, "12.0.0", s.resolveMattermostServerVersion(),
			"empty E2EServerVersion must fall back to latest resolution, not return empty")
	})

	t.Run("whitespace-only config falls back to latest resolution", func(t *testing.T) {
		// Defensive: treat whitespace as empty for the same reason — a config-edit
		// typo with stray whitespace should not silently break provisioning.
		body := `[{"tag_name":"v12.0.0","draft":false,"prerelease":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServer(t, "", "mattermost")
		s.Config.E2EServerVersion = "   "
		s.githubAPIBase = srv.URL + "/"

		assert.Equal(t, "12.0.0", s.resolveMattermostServerVersion())
	})

	t.Run("RC tags included, highest semver returned", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-rc2","draft":false},
			{"tag_name":"v11.7.0-rc1","draft":false},
			{"tag_name":"v11.6.0","draft":false},
			{"tag_name":"v11.5.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.7.0-rc2", s.resolveMattermostServerVersion())
	})

	t.Run("beta tags skipped, stable returned", func(t *testing.T) {
		// 12.0.0-beta.1 would beat 11.9.0-rc1 in semver but must be excluded —
		// beta builds are too early for E2E infra.
		body := `[
			{"tag_name":"v12.0.0-beta.1","draft":false},
			{"tag_name":"v11.9.0-rc1","draft":false},
			{"tag_name":"v11.8.1","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.9.0-rc1", s.resolveMattermostServerVersion())
	})

	t.Run("alpha tags skipped, RC of lower version returned", func(t *testing.T) {
		// 12.0.0-alpha.1 would beat 11.9.0-rc1 in semver but must be excluded —
		// alpha builds are too early for E2E infra. Mattermost published v11.0.0-alpha.1
		// as a one-off for the major-version announcement; v12.0.0-alpha.1 is plausible.
		body := `[
			{"tag_name":"v12.0.0-alpha.1","draft":false},
			{"tag_name":"v11.9.0-rc1","draft":false},
			{"tag_name":"v11.8.1","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.9.0-rc1", s.resolveMattermostServerVersion())
	})

	t.Run("draft releases skipped", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0","draft":true},
			{"tag_name":"v11.6.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveMattermostServerVersion())
	})

	t.Run("stable release at top of list returned immediately", func(t *testing.T) {
		body := `[{"tag_name":"v12.0.0","draft":false},{"tag_name":"v11.6.0","draft":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "12.0.0", s.resolveMattermostServerVersion())
	})

	t.Run("tag without v prefix returned unchanged", func(t *testing.T) {
		// TrimPrefix("v", non-v-string) is a no-op — bare semver tags work too.
		body := `[{"tag_name":"11.6.0","draft":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveMattermostServerVersion())
	})

	t.Run("only RCs in list → returns highest RC", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-rc1","draft":false},
			{"tag_name":"v11.6.0-rc2","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.7.0-rc1", s.resolveMattermostServerVersion())
	})

	t.Run("only drafts in list → fallback to master", func(t *testing.T) {
		body := `[{"tag_name":"v11.7.0","draft":true}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveMattermostServerVersion())
	})

	t.Run("empty releases list → fallback to master", func(t *testing.T) {
		srv := mockReleasesServer(t, `[]`, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveMattermostServerVersion())
	})

	t.Run("API returns 500 → fallback to master", func(t *testing.T) {
		srv := mockReleasesServer(t, `{"message":"Internal Server Error"}`, http.StatusInternalServerError)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveMattermostServerVersion())
	})

	t.Run("mixed: draft RC then non-draft RC and stable → returns highest non-draft", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-rc1","draft":true},
			{"tag_name":"v11.7.0-rc1","draft":false},
			{"tag_name":"v11.6.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.7.0-rc1", s.resolveMattermostServerVersion())
	})

	t.Run("prerelease flag no longer skipped — highest semver returned", func(t *testing.T) {
		// prerelease flag is ignored; only draft is excluded.
		body := `[
			{"tag_name":"v11.6.0","draft":false,"prerelease":true},
			{"tag_name":"v11.5.0","draft":false,"prerelease":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveMattermostServerVersion())
	})

	t.Run("prerelease flag and rc tag both included, highest semver returned", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0","draft":false,"prerelease":true},
			{"tag_name":"v11.6.1-rc1","draft":false,"prerelease":false},
			{"tag_name":"v11.6.0","draft":false,"prerelease":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.7.0", s.resolveMattermostServerVersion())
	})

	t.Run("resolved version is cached — API called only once", func(t *testing.T) {
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/releases") {
				callCount++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[{"tag_name":"v11.6.0","draft":false,"prerelease":false}]`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServerLatest(t, srv)

		v1 := s.resolveMattermostServerVersion()
		v2 := s.resolveMattermostServerVersion()
		v3 := s.resolveMattermostServerVersion()

		assert.Equal(t, "11.6.0", v1)
		assert.Equal(t, "11.6.0", v2)
		assert.Equal(t, "11.6.0", v3)
		assert.Equal(t, 1, callCount, "GitHub API must be called exactly once; subsequent calls use the cache")
	})

	t.Run("stale cache returned when API fails on refresh", func(t *testing.T) {
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/releases") {
				callCount++
				if callCount == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`[{"tag_name":"v11.6.0","draft":false}]`))
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServerLatest(t, srv)

		v1 := s.resolveMattermostServerVersion() // populates cache with "11.6.0"
		assert.Equal(t, "11.6.0", v1)

		// Expire the cache so the next call hits the API again.
		s.e2eVersionCacheLock.Lock()
		s.e2eVersionCacheTime = s.e2eVersionCacheTime.Add(-2 * time.Hour)
		s.e2eVersionCacheLock.Unlock()

		v2 := s.resolveMattermostServerVersion() // API fails → stale cache returned
		assert.Equal(t, "11.6.0", v2, "should return last known version when API fails on cache refresh")
		assert.Equal(t, 2, callCount)
	})

	t.Run("fallback master is not cached — retried on next call", func(t *testing.T) {
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/releases") {
				callCount++
				// First call fails; second call succeeds.
				if callCount == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[{"tag_name":"v11.6.0","draft":false,"prerelease":false}]`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServerLatest(t, srv)

		v1 := s.resolveMattermostServerVersion() // API error → fallback "master" (not cached)
		v2 := s.resolveMattermostServerVersion() // retried → "11.6.0" (now cached)
		v3 := s.resolveMattermostServerVersion() // cache hit → "11.6.0"

		assert.Equal(t, "master", v1, "first call: API error should fall back to master")
		assert.Equal(t, "11.6.0", v2, "second call: should retry and resolve correctly")
		assert.Equal(t, "11.6.0", v3, "third call: should return cached value")
		assert.Equal(t, 2, callCount, "API should be called twice: once for the failed attempt, once for the retry")
	})
}

// ------------------------------------------------------------
// 12b. Push-event server version selection — always latest, ignore branch suffix
// ------------------------------------------------------------

func TestDryRun_PushEventServerVersion(t *testing.T) {
	// Push events (release branch, master, main) must always provision the latest
	// stable Mattermost release. Deriving a version from a release branch name
	// (e.g. "release-9.0" → "9.0") would attempt to pull a Docker tag that
	// typically doesn't exist (Docker Hub publishes full SemVer like "9.0.0"),
	// causing silent installation failures and no E2E workflow dispatch.
	t.Run("release branch push uses latest version, ignoring branch-derived version", func(t *testing.T) {
		body := `[{"tag_name":"v12.0.0","draft":false,"prerelease":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)

		assert.Equal(t, "12.0.0", s.serverVersionForPushEvent(),
			"release-9.0 push must provision latest server (12.0.0), not branch-derived 9.0")
	})

	t.Run("master push uses latest version", func(t *testing.T) {
		body := `[{"tag_name":"v12.0.0","draft":false,"prerelease":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)

		assert.Equal(t, "12.0.0", s.serverVersionForPushEvent())
	})
}

// ------------------------------------------------------------
// 13. MM_SERVER_VERSION sourced from instance, not config
// ------------------------------------------------------------

func TestDryRun_MMServerVersionFromInstance(t *testing.T) {
	t.Run("desktop dispatch uses instances[0].ServerVersion not config", func(t *testing.T) {
		// Config says "latest" but instances were provisioned with resolved "11.6.0".
		// Drive triggerDesktopE2EWorkflow so we assert the real payload, not a hand-built map.
		s := newDryRunServer(t, "", "mattermost")
		s.Config.E2EServerVersion = "latest" // would be wrong if used in dispatch

		resolvedVersion := "11.6.0"
		instances := []*E2EInstance{
			{Name: "inst-linux", Platform: "linux", Runner: "ubuntu-latest",
				URL: "https://linux.test.example.com", InstallationID: "id-1",
				ServerVersion: resolvedVersion},
			{Name: "inst-macos", Platform: "macos", Runner: "macos-latest",
				URL: "https://macos.test.example.com", InstallationID: "id-2",
				ServerVersion: resolvedVersion},
			{Name: "inst-windows", Platform: "windows", Runner: "windows-2022",
				URL: "https://windows.test.example.com", InstallationID: "id-3",
				ServerVersion: resolvedVersion},
		}

		ghSrv, captures := mockGitHubServer(t, http.StatusNoContent)
		client := newTestGitHubClient(t, ghSrv)
		pr := &model.PullRequest{
			RepoOwner: "mattermost",
			RepoName:  "mattermost-desktop",
			Number:    99,
			Ref:       "feature-branch",
			Sha:       "abc123",
		}

		err := s.triggerDesktopE2EWorkflow(context.Background(), client, pr, instances)
		require.NoError(t, err)
		require.Len(t, *captures, 1)

		c := (*captures)[0]
		assert.Equal(t, "11.6.0", c.Inputs["MM_SERVER_VERSION"],
			"MM_SERVER_VERSION must be the resolved instance version, not the 'latest' config sentinel")
		assert.NotEqual(t, s.Config.E2EServerVersion, c.Inputs["MM_SERVER_VERSION"],
			"MM_SERVER_VERSION must NOT be the raw config value when config is 'latest'")
	})

	t.Run("mobile dispatch does not include MM_SERVER_VERSION", func(t *testing.T) {
		// Drive triggerMobileE2EWorkflow so we assert the real payload, not a hand-built map.
		s := newDryRunServer(t, "", "mattermost")
		instances := makeMobileInstances()
		for _, instance := range instances {
			instance.ServerVersion = "11.6.0"
		}

		ghSrv, captures := mockGitHubServer(t, http.StatusNoContent)
		client := newTestGitHubClient(t, ghSrv)
		pr := &model.PullRequest{
			RepoOwner: "mattermost",
			RepoName:  "mattermost-mobile",
			Number:    99,
			Ref:       "feature-branch",
			Sha:       "feature-sha",
		}

		err := s.triggerMobileE2EWorkflow(context.Background(), client, pr, instances, "both")
		require.NoError(t, err)
		require.Len(t, *captures, 1)

		c := (*captures)[0]
		assert.NotContains(t, c.Inputs, "MM_SERVER_VERSION",
			"mobile dispatch must never include MM_SERVER_VERSION")
		assert.NotContains(t, c.Inputs, "instance_details",
			"mobile dispatch must never include instance_details")
		assert.Equal(t, "https://android-site1.test.example.com", c.Inputs["ANDROID_SITE_1_URL"])
		assert.Equal(t, "https://android-site2.test.example.com", c.Inputs["ANDROID_SITE_2_URL"])
		assert.Equal(t, "https://ios-site1.test.example.com", c.Inputs["IOS_SITE_1_URL"])
		assert.Equal(t, "https://ios-site2.test.example.com", c.Inputs["IOS_SITE_2_URL"])
		assert.Equal(t, "https://site3.test.example.com", c.Inputs["SITE_3_URL"])
	})

	t.Run("resolveMattermostServerVersion with latest returns Docker Hub compatible version", func(t *testing.T) {
		// Docker Hub tags are bare semver (e.g. "11.6.0"), NOT "v11.6.0".
		// Verify the v-stripping produces a Docker Hub compatible string.
		body := `[{"tag_name":"v11.6.0","draft":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)

		version := s.resolveMattermostServerVersion()
		assert.Equal(t, "11.6.0", version)
		assert.False(t, strings.HasPrefix(version, "v"),
			"resolved version must NOT have 'v' prefix — Docker Hub tags are bare semver")
	})
}

// ------------------------------------------------------------
// 14. CMT version normalization (v-prefix stripping)
// ------------------------------------------------------------

func TestDryRun_CMTVersionNormalization(t *testing.T) {
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

// ------------------------------------------------------------
// 15. resolveCMTServerVersions() — auto-derived CMT version set
// ------------------------------------------------------------

func TestDryRun_ResolveCMTServerVersions(t *testing.T) {
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

// TestResolveBranchHeadSHA verifies the dispatch-time HEAD resolution used to key non-PR
// cleanup. Non-PR flows dispatch the test workflow with ref=branch, so the run's head_sha is
// the branch HEAD at dispatch time. We key cleanup on that resolved SHA (not the trigger SHA)
// so findAndDestroyInstancesBySHA matches when the run completes.
func TestResolveBranchHeadSHA(t *testing.T) {
	t.Run("returns the branch HEAD sha from the commits API", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sha":"abc123def456"}`))
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		sha, err := s.resolveBranchHeadSHA("mattermost", "desktop", "release-12.0")
		assert.NoError(t, err)
		assert.Equal(t, "abc123def456", sha)
		assert.Equal(t, "/repos/mattermost/desktop/commits/release-12.0", gotPath)
	})

	t.Run("errors on non-2xx so caller can fall back to trigger sha", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		_, err := s.resolveBranchHeadSHA("mattermost", "desktop", "no-such-branch")
		assert.Error(t, err)
	})

	t.Run("errors on empty sha so caller can fall back to trigger sha", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sha":""}`))
		}))
		t.Cleanup(srv.Close)

		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		_, err := s.resolveBranchHeadSHA("mattermost", "desktop", "main")
		assert.Error(t, err)
	})
}

// TestE2EPRInstanceMaxAge verifies the PR max-age knob: configured value wins, else 24h default.
func TestE2EPRInstanceMaxAge(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")

	s.Config.E2EPRInstanceMaxAge = 0
	assert.Equal(t, 8*time.Hour, s.e2ePRInstanceMaxAge(), "0 should fall back to the 8h default in config-matterwick.default.json")

	s.Config.E2EPRInstanceMaxAge = 48
	assert.Equal(t, 48*time.Hour, s.e2ePRInstanceMaxAge(), "configured value should win")
}

// TestEvictReapedPRInstances verifies that when the periodic scan reaps a PR's servers, the
// in-memory tracking entry is removed so the next E2E/Run provisions a fresh set rather than
// reusing now-deleted servers. Non-PR (SHA-keyed) entries must be left untouched.
func TestEvictReapedPRInstances(t *testing.T) {
	t.Run("evicts the PR key when any of its instances was reaped", func(t *testing.T) {
		s := newDryRunServer(t, "", "mattermost")
		s.e2eInstances["desktop-pr-42"] = []*E2EInstance{
			{InstallationID: "inst-a", Platform: "linux"},
			{InstallationID: "inst-b", Platform: "macos"},
			{InstallationID: "inst-c", Platform: "windows"},
		}

		// Only one member reaped, but the whole set ages out together, so the key goes.
		s.evictReapedPRInstances([]string{"inst-b"}, s.Logger)

		_, ok := s.e2eInstances["desktop-pr-42"]
		assert.False(t, ok, "PR key must be evicted so re-applying E2E/Run creates a fresh set")
	})

	t.Run("leaves unrelated PR keys and non-PR (SHA-keyed) entries intact", func(t *testing.T) {
		s := newDryRunServer(t, "", "mattermost")
		s.e2eInstances["desktop-pr-42"] = []*E2EInstance{{InstallationID: "inst-a"}}
		s.e2eInstances["mattermost-mobile-pr-9"] = []*E2EInstance{{InstallationID: "inst-x"}}
		s.e2eInstances["desktop-cmt-555-deadbeef"] = []*E2EInstance{{InstallationID: "inst-cmt"}}

		s.evictReapedPRInstances([]string{"inst-a"}, s.Logger)

		_, gone := s.e2eInstances["desktop-pr-42"]
		assert.False(t, gone, "the matching PR key is evicted")
		_, otherPR := s.e2eInstances["mattermost-mobile-pr-9"]
		assert.True(t, otherPR, "an unrelated PR key must remain")
		_, cmt := s.e2eInstances["desktop-cmt-555-deadbeef"]
		assert.True(t, cmt, "a non-PR (SHA-keyed) entry must remain")
	})

	t.Run("no reaped IDs is a no-op", func(t *testing.T) {
		s := newDryRunServer(t, "", "mattermost")
		s.e2eInstances["desktop-pr-42"] = []*E2EInstance{{InstallationID: "inst-a"}}

		s.evictReapedPRInstances(nil, s.Logger)

		_, ok := s.e2eInstances["desktop-pr-42"]
		assert.True(t, ok, "nothing reaped means nothing evicted")
	})
}

// TestShouldTriggerCMT verifies CMT gating across all sources: manual dispatch (any ref), RC
// tag cut (the new primary trigger), and release branch (defense-in-depth). Anything else —
// feature branches, GA tags, nightly tags, beta tags, default branch — must be rejected so
// that mobile's `on: push tags` glob slips and stray runs don't burn the multi-version matrix.
func TestShouldTriggerCMT(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")

	// Manual dispatch always runs, regardless of ref.
	assert.True(t, s.shouldTriggerCMT("workflow_dispatch", "main"))
	assert.True(t, s.shouldTriggerCMT("workflow_dispatch", "v6.2.0-rc.1"))
	assert.True(t, s.shouldTriggerCMT("workflow_dispatch", "release-6.2"))

	// RC tag cut (primary trigger). For tag pushes head_branch is the tag name.
	assert.True(t, s.shouldTriggerCMT("push", "v6.2.0-rc.1"))  // desktop convention
	assert.True(t, s.shouldTriggerCMT("push", "v2.41.0-rc.1")) // future mobile
	assert.True(t, s.shouldTriggerCMT("push", "v6.2.0-rc.10")) // multi-digit rc
	assert.True(t, s.shouldTriggerCMT("push", "6.2.0-rc.1"))   // missing 'v' prefix is permitted
	assert.True(t, s.shouldTriggerCMT("push", "v6.2.0-rc1"))   // no separator before number

	// Release branch (defense-in-depth — kept for backwards compat / manual triggers).
	assert.True(t, s.shouldTriggerCMT("push", "release-6.2"))

	// Must NOT trigger: GA tags, betas, nightly tags, feature branches, default branch.
	assert.False(t, s.shouldTriggerCMT("push", "v6.2.0"))                 // GA tag — no -rc
	assert.False(t, s.shouldTriggerCMT("push", "v1.0.22-beta"))           // pre-release but not RC
	assert.False(t, s.shouldTriggerCMT("push", "6.3.0-nightly.20260601")) // nightly tag
	assert.False(t, s.shouldTriggerCMT("push", "v6.2.0-rcabc"))           // -rc but no number
	assert.False(t, s.shouldTriggerCMT("create", "feature/cool-thing"))
	assert.False(t, s.shouldTriggerCMT("push", "main"))
	assert.False(t, s.shouldTriggerCMT("schedule", "main"))

	// Mobile build-release-NNN branch push (mobile's RC-cut equivalent).
	assert.True(t, s.shouldTriggerCMT("push", "build-release-786"))      // 3-digit
	assert.True(t, s.shouldTriggerCMT("push", "build-release-1100"))     // 4-digit
	assert.False(t, s.shouldTriggerCMT("push", "build-release-12345"))   // 5+ digit rejected; bump regex when convention changes
	assert.False(t, s.shouldTriggerCMT("push", "build-release-1"))       // < 3 digits, likely a test
	assert.False(t, s.shouldTriggerCMT("push", "build-release-ios-707")) // platform variant
}

// TestIsRCTag covers the RC-tag regex in isolation so the boundary cases stay locked in.
func TestIsRCTag(t *testing.T) {
	for _, ref := range []string{"v6.2.0-rc.1", "v6.2.0-rc.10", "v2.41.0-rc.2", "6.2.0-rc.1", "v6.2.0-rc1", "v6.2.0-rc-1"} {
		assert.True(t, isRCTag(ref), "expected RC tag: %q", ref)
	}
	for _, ref := range []string{
		"v6.2.0",                 // GA
		"v6.2.0-rc",              // missing number
		"v6.2.0-rcabc",           // letters after -rc
		"v1.0.22-beta",           // not RC
		"6.3.0-nightly.20260601", // nightly
		"release-6.2",            // branch
		"main",
		"",
	} {
		assert.False(t, isRCTag(ref), "must not match: %q", ref)
	}
}

// TestIsBuildReleaseBranch locks in the boundaries for mobile's build-release-NNN gate:
// exactly 3-or-4 digits required (rejects test artifacts like build-release-1 AND
// rejects unsanctioned 5+ digit values); platform-specific variants
// (build-release-ios-NNN etc.) are rejected; no overlap with the RC-tag or
// release-* gates.
func TestIsBuildReleaseBranch(t *testing.T) {
	for _, ref := range []string{
		"build-release-786",  // real 3-digit
		"build-release-1100", // real 4-digit
		"build-release-9999", // upper end of 4-digit window
	} {
		assert.True(t, isBuildReleaseBranch(ref), "expected build-release branch: %q", ref)
	}
	for _, ref := range []string{
		"build-release-1",            // 1 digit — test artifact / typo
		"build-release-99",           // 2 digit — below convention
		"build-release-12345",        // 5 digit — above convention; bump regex when crossing this
		"build-release-ios-707",      // platform-specific
		"build-release-sim-707",      // simulator-only
		"build-release-android-1100", // android-specific
		"build-release-786-rc1",      // stray suffix
		"build-release-",             // no number
		"build-release-abc",          // non-numeric
		"release-2.41",               // handled by isReleaseBranch
		"v2.41.0-rc.1",               // handled by isRCTag
		"",
	} {
		assert.False(t, isBuildReleaseBranch(ref), "must not match: %q", ref)
	}
}

// TestDryRun_MobileCMTVersionSelection covers the mobile version set: newest ESR, latest
// production (newest non-ESR stable), and the current RC — one topology of five servers each.
func TestDryRun_MobileCMTVersionSelection(t *testing.T) {
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

	t.Run("API error falls back to the default set", func(t *testing.T) {
		srv := mockReleasesServer(t, "boom", http.StatusInternalServerError)
		s := newDryRunServer(t, "", "mattermost")
		s.githubAPIBase = srv.URL + "/"

		assert.Equal(t, defaultCMTServerVersions, s.resolveMobileCMTServerVersions())
		assert.LessOrEqual(t, len(defaultCMTServerVersions), maxMobileCMTServerVersions)
	})
}

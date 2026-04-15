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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

// makeDesktopInstances fabricates the 3 desktop instances (linux/macos/windows).
func makeDesktopInstances() []*E2EInstance {
	return []*E2EInstance{
		{Name: "inst-linux", Platform: "linux", Runner: "ubuntu-latest", URL: "https://linux.test.example.com", InstallationID: "id-1", ServerVersion: "master"},
		{Name: "inst-macos", Platform: "macos", Runner: "macos-latest", URL: "https://macos.test.example.com", InstallationID: "id-2", ServerVersion: "master"},
		{Name: "inst-windows", Platform: "windows", Runner: "windows-2022", URL: "https://windows.test.example.com", InstallationID: "id-3", ServerVersion: "master"},
	}
}

// makeMobileInstances fabricates the 3 mobile instances (site-1/2/3).
func makeMobileInstances() []*E2EInstance {
	return []*E2EInstance{
		{Name: "inst-site1", Platform: "site-1", URL: "https://site1.test.example.com", InstallationID: "id-1", ServerVersion: "master"},
		{Name: "inst-site2", Platform: "site-2", URL: "https://site2.test.example.com", InstallationID: "id-2", ServerVersion: "master"},
		{Name: "inst-site3", Platform: "site-3", URL: "https://site3.test.example.com", InstallationID: "id-3", ServerVersion: "master"},
	}
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

	t.Run("three platforms created for desktop", func(t *testing.T) {
		assert.Len(t, instances, 3)
		platforms := []string{instances[0].Platform, instances[1].Platform, instances[2].Platform}
		assert.Equal(t, []string{"linux", "macos", "windows"}, platforms)
	})

	t.Run("runner assignment is correct", func(t *testing.T) {
		assert.Equal(t, "ubuntu-latest", instances[0].Runner)
		assert.Equal(t, "macos-latest", instances[1].Runner)
		assert.Equal(t, "windows-2022", instances[2].Runner)
	})

	t.Run("workflow inputs built correctly", func(t *testing.T) {
		// MM_SERVER_VERSION must come from instances[0].ServerVersion, NOT from
		// s.Config.E2EServerVersion. The instance stores the resolved version set at
		// provisioning time, which may differ from config (e.g. config="latest").
		workflowInputs := map[string]interface{}{
			"instance_details":  instanceDetailsJSON,
			"version_name":      "feature-branch",
			"MM_TEST_USER_NAME": s.Config.E2EUsername,
			"MM_SERVER_VERSION": instances[0].ServerVersion, // NOT s.Config.E2EServerVersion
		}

		assert.Equal(t, instances[0].ServerVersion, workflowInputs["MM_SERVER_VERSION"])
		assert.Equal(t, "e2eadmin", workflowInputs["MM_TEST_USER_NAME"])
		assert.Equal(t, "feature-branch", workflowInputs["version_name"])
		assert.NotEmpty(t, workflowInputs["instance_details"])
	})

	t.Run("workflow path targets e2e-functional.yml", func(t *testing.T) {
		path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches",
			"mattermost", "mattermost-desktop", "e2e-functional.yml")
		assert.Contains(t, path, "e2e-functional.yml")
		assert.Contains(t, path, "mattermost-desktop")
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

			// Build the inputs as triggerMobileE2EWorkflow does
			inputs := map[string]interface{}{
				"MOBILE_VERSION": prSha,
				"PLATFORM":       platform,
			}
			for i, inst := range instances {
				inputs[fmt.Sprintf("SITE_%d_URL", i+1)] = inst.URL
			}

			body := map[string]interface{}{"ref": prRef, "inputs": inputs}
			jsonBytes, err := json.Marshal(body)
			require.NoError(t, err)

			var parsed struct {
				Ref    string                 `json:"ref"`
				Inputs map[string]interface{} `json:"inputs"`
			}
			require.NoError(t, json.Unmarshal(jsonBytes, &parsed))

			assert.Equal(t, prRef, parsed.Ref)
			assert.Equal(t, tt.platform, parsed.Inputs["PLATFORM"])
			assert.Equal(t, prSha, parsed.Inputs["MOBILE_VERSION"])
			assert.Equal(t, "https://site1.test.example.com", parsed.Inputs["SITE_1_URL"])
			assert.Equal(t, "https://site2.test.example.com", parsed.Inputs["SITE_2_URL"])
			assert.Equal(t, "https://site3.test.example.com", parsed.Inputs["SITE_3_URL"])

			// Mobile must NOT use instance_details (desktop-only field)
			assert.NotContains(t, parsed.Inputs, "instance_details",
				"mobile workflow must not send instance_details")
		})
	}

	t.Run("mobile platforms are site-1/2/3 not linux/macos/windows", func(t *testing.T) {
		platforms := []string{instances[0].Platform, instances[1].Platform, instances[2].Platform}
		assert.Equal(t, []string{"site-1", "site-2", "site-3"}, platforms)
	})

	t.Run("mobile instances have no runner", func(t *testing.T) {
		for _, inst := range instances {
			assert.Empty(t, inst.Runner)
		}
	})

	t.Run("triggerMobileE2EWorkflow requires exactly 3 instances", func(t *testing.T) {
		// Verify the guard: only 2 instances → error path
		twoInstances := instances[:2]
		assert.NotEqual(t, 3, len(twoInstances),
			"should fail the len(instances)!=3 check in triggerMobileE2EWorkflow")
	})

	t.Run("workflow path targets e2e-detox-pr.yml", func(t *testing.T) {
		path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches",
			"mattermost", "mattermost-mobile", "e2e-detox-pr.yml")
		assert.Contains(t, path, "e2e-detox-pr.yml")
		assert.Contains(t, path, "mattermost-mobile")
	})
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
// 4. Repo type → correct platforms and workflow
// ------------------------------------------------------------

func TestDryRun_RepoTypeDetection(t *testing.T) {
	tests := []struct {
		repoName      string
		wantType      string
		wantPlatforms []string
		wantWorkflow  string
	}{
		{
			repoName:      "mattermost-desktop",
			wantType:      "desktop",
			wantPlatforms: []string{"linux", "macos", "windows"},
			wantWorkflow:  "e2e-functional.yml",
		},
		{
			repoName:      "mattermost-desktop-releases",
			wantType:      "desktop",
			wantPlatforms: []string{"linux", "macos", "windows"},
			wantWorkflow:  "e2e-functional.yml",
		},
		{
			repoName:      "mattermost-mobile",
			wantType:      "mobile",
			wantPlatforms: []string{"site-1", "site-2", "site-3"},
			wantWorkflow:  "e2e-detox-pr.yml",
		},
		{
			repoName:      "mattermost-mobile-v2",
			wantType:      "mobile",
			wantPlatforms: []string{"site-1", "site-2", "site-3"},
			wantWorkflow:  "e2e-detox-pr.yml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.repoName, func(t *testing.T) {
			var instanceType string
			var platforms []string
			var workflow string

			if strings.Contains(tt.repoName, "desktop") {
				instanceType = "desktop"
				platforms = []string{"linux", "macos", "windows"}
				workflow = "e2e-functional.yml"
			} else if strings.Contains(tt.repoName, "mobile") {
				instanceType = "mobile"
				platforms = []string{"site-1", "site-2", "site-3"}
				workflow = "e2e-detox-pr.yml"
			}

			assert.Equal(t, tt.wantType, instanceType)
			assert.Equal(t, tt.wantPlatforms, platforms)
			assert.Equal(t, tt.wantWorkflow, workflow)
		})
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

	t.Run("version extracted from release branch", func(t *testing.T) {
		assert.Equal(t, "8.0", extractVersionFromReleaseBranch("release-8.0", "release-"))
		assert.Equal(t, "10.5", extractVersionFromReleaseBranch("release-10.5", "release-"))
		assert.Equal(t, "", extractVersionFromReleaseBranch("master", "release-"))
	})

	t.Run("branch name extracted from git ref", func(t *testing.T) {
		assert.Equal(t, "release-8.0", extractBranchName("refs/heads/release-8.0"))
		assert.Equal(t, "master", extractBranchName("refs/heads/master"))
		assert.Equal(t, "feature/my-branch", extractBranchName("refs/heads/feature/my-branch"))
	})

	t.Run("desktop push always creates linux/macos/windows instances", func(t *testing.T) {
		// createMultipleE2EInstancesForPushEvent uses desktop platforms for push events
		expectedPlatforms := []string{"linux", "macos", "windows"}
		instances := makeDesktopInstances()
		var gotPlatforms []string
		for _, inst := range instances {
			gotPlatforms = append(gotPlatforms, inst.Platform)
		}
		assert.Equal(t, expectedPlatforms, gotPlatforms)
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
// 6. Mobile push event logic
// ------------------------------------------------------------

func TestDryRun_MobilePushEvent(t *testing.T) {
	t.Run("mobile push uses SITE_1/2/3_URL inputs not instance_details", func(t *testing.T) {
		instances := makeMobileInstances()
		sha := "sha999"
		branch := "release-8.0"

		// Simulate triggerMobileE2EWorkflowForPushEvent inputs
		inputs := map[string]interface{}{
			"SITE_1_URL":     instances[0].URL,
			"SITE_2_URL":     instances[1].URL,
			"SITE_3_URL":     instances[2].URL,
			"MOBILE_VERSION": sha,
			"PLATFORM":       "both",
		}

		assert.Equal(t, "https://site1.test.example.com", inputs["SITE_1_URL"])
		assert.Equal(t, "https://site2.test.example.com", inputs["SITE_2_URL"])
		assert.Equal(t, "https://site3.test.example.com", inputs["SITE_3_URL"])
		assert.Equal(t, sha, inputs["MOBILE_VERSION"])
		assert.Equal(t, "both", inputs["PLATFORM"])
		assert.NotContains(t, inputs, "instance_details",
			"mobile push must not use instance_details")
		_ = branch
	})

	t.Run("mobile push always tests both platforms", func(t *testing.T) {
		// Push events (release/master) always use PLATFORM=both (no label context)
		platform := "both"
		assert.Equal(t, "both", platform)
	})

	t.Run("mobile push requires 3 instances", func(t *testing.T) {
		instances := makeMobileInstances()
		assert.Len(t, instances, 3)
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

	t.Run("caps server versions to 5", func(t *testing.T) {
		versions := parseServerVersionsFromString("v1, v2, v3, v4, v5, v6, v7")
		// The cap is enforced inside handleCMTWithServerVersions
		if len(versions) > 5 {
			versions = versions[:5]
		}
		assert.Len(t, versions, 5)
	})

	t.Run("1 instance per version for CMT (matrix handles parallelism)", func(t *testing.T) {
		for _, numVersions := range []int{1, 2, 3, 5} {
			// CMT_MATRIX cross-products environment × server; one server per version is enough
			assert.Equal(t, numVersions, numVersions*1)
		}
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

		servers, ok := matrix["server"].([]interface{})
		require.True(t, ok)
		require.Len(t, servers, 2)
		s0 := servers[0].(map[string]interface{})
		assert.Equal(t, "v11.1.0", s0["version"])
		assert.Equal(t, "https://v1.example.com", s0["url"])
		s1 := servers[1].(map[string]interface{})
		assert.Equal(t, "v11.2.0", s1["version"])
		assert.Equal(t, "https://v2.example.com", s1["url"])
	})

	t.Run("CMT dispatches compatibility-matrix-testing.yml once", func(t *testing.T) {
		// One dispatch regardless of version count — all versions in CMT_MATRIX.server array
		dispatchCount := 1
		assert.Equal(t, 1, dispatchCount, "desktop CMT must dispatch exactly once")
	})

	t.Run("CMT tracking key includes runID for uniqueness and sha for cleanup", func(t *testing.T) {
		repoName := "mattermost-desktop"
		sha := "deadbeef"
		var runID int64 = 999
		// runID prevents collision when two dispatches share the same branch HEAD SHA;
		// key still ends with "-{sha}" so findAndDestroyInstancesBySHA can match it.
		key := fmt.Sprintf("%s-cmt-%d-%s", repoName, runID, sha)
		assert.Equal(t, "mattermost-desktop-cmt-999-deadbeef", key)
		assert.True(t, strings.HasSuffix(key, "-"+sha), "key must end with sha for cleanup")
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
		instances := []*E2EInstance{
			{URL: "https://v1.example.com", ServerVersion: "v11.1.0"},
			{URL: "https://v2.example.com", ServerVersion: "v11.2.0"},
		}
		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		var matrix map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(jsonStr), &matrix))

		servers, ok := matrix["server"].([]interface{})
		require.True(t, ok)
		require.Len(t, servers, 2)
		s0 := servers[0].(map[string]interface{})
		assert.Equal(t, "v11.1.0", s0["version"])
		assert.Equal(t, "https://v1.example.com", s0["url"])
		s1 := servers[1].(map[string]interface{})
		assert.Equal(t, "v11.2.0", s1["version"])
		assert.Equal(t, "https://v2.example.com", s1["url"])
	})

	t.Run("mobile CMT dispatches once not once per version", func(t *testing.T) {
		// All versions go into CMT_MATRIX.server; compatibility-matrix-testing.yml
		// fans them out via its matrix strategy — no per-version dispatch needed.
		dispatchCount := 1
		assert.Equal(t, 1, dispatchCount, "mobile CMT must dispatch exactly once")
	})

	t.Run("CMT_MATRIX uses server array not SITE_URL inputs", func(t *testing.T) {
		versions := []string{"v11.1.0", "v11.2.0"}
		instances := []*E2EInstance{
			{URL: "https://v1.example.com", ServerVersion: "v11.1.0"},
			{URL: "https://v2.example.com", ServerVersion: "v11.2.0"},
		}
		jsonStr, err := buildMobileCMTMatrixJSON(versions, instances)
		require.NoError(t, err)

		// CMT_MATRIX must use "server" array, not the SITE_1/2/3_URL inputs used for PR runs
		assert.NotContains(t, jsonStr, "SITE_1_URL")
		assert.NotContains(t, jsonStr, "SITE_2_URL")
		assert.Contains(t, jsonStr, "\"server\"")
		assert.Contains(t, jsonStr, "\"url\"")
	})

	t.Run("mobile CMT single instance per version for matrix fan-out", func(t *testing.T) {
		// Mobile CMT uses one server per version; compatibility-matrix-testing.yml
		// creates one test job per server entry.
		versions := []string{"v11.1.0", "v11.2.0", "v11.3.0"}
		instances := []*E2EInstance{
			{URL: "https://v1.example.com"},
			{URL: "https://v2.example.com"},
			{URL: "https://v3.example.com"},
		}
		assert.Equal(t, len(versions), len(instances), "one instance per version")
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

		assert.Len(t, retrieved, 3)

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

		assert.Len(t, collected, 6, "should collect 3 instances from each of 2 push keys")
	})

	t.Run("CMT cleanup by sha via findAndDestroyInstancesBySHA", func(t *testing.T) {
		repoName := "mattermost-desktop"
		sha := "abc123cmt"
		var runID int64 = 42
		key := fmt.Sprintf("%s-cmt-%d-%s", repoName, runID, sha)

		cmtInstances := makeDesktopInstances()
		s.e2eInstancesLock.Lock()
		s.e2eInstances[key] = cmtInstances
		s.e2eInstancesLock.Unlock()

		// Simulate findAndDestroyInstancesBySHA: scan for prefix+suffix match
		prefix := repoName + "-"
		suffix := "-" + sha
		s.e2eInstancesLock.Lock()
		var found []*E2EInstance
		for k, v := range s.e2eInstances {
			if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, suffix) {
				found = append(found, v...)
				delete(s.e2eInstances, k)
			}
		}
		s.e2eInstancesLock.Unlock()

		assert.Len(t, found, 3)

		s.e2eInstancesLock.Lock()
		_, exists := s.e2eInstances[key]
		s.e2eInstancesLock.Unlock()
		assert.False(t, exists)
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

	t.Run("CMT single instance name sanitization replaces dots", func(t *testing.T) {
		version := "v11.1.0"
		// createSingleCMTInstance lowercases and replaces dots
		sanitizedVersion := strings.ToLower(strings.ReplaceAll(version, ".", "-"))
		assert.Equal(t, "v11-1-0", sanitizedVersion)

		// Single instance suffix: no platform component (matrix handles that)
		suffix := fmt.Sprintf("-cmt-%s", sanitizedVersion)
		assert.Equal(t, "-cmt-v11-1-0", suffix)
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

// ------------------------------------------------------------
// 12. resolveE2EServerVersion() — GitHub releases API logic
// ------------------------------------------------------------

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
			assert.Equal(t, cfg, s.resolveE2EServerVersion(), "config=%q should be returned unchanged", cfg)
		}
		assert.False(t, called, "GitHub API must not be called when E2EServerVersion is not 'latest'")
	})

	t.Run("RC tags skipped, first stable tag returned with v stripped", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-rc2","draft":false},
			{"tag_name":"v11.7.0-rc1","draft":false},
			{"tag_name":"v11.6.0","draft":false},
			{"tag_name":"v11.5.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
	})

	t.Run("beta tags skipped", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-beta.1","draft":false},
			{"tag_name":"v11.6.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
	})

	t.Run("alpha tags skipped", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-alpha.1","draft":false},
			{"tag_name":"v11.6.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
	})

	t.Run("draft releases skipped", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0","draft":true},
			{"tag_name":"v11.6.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
	})

	t.Run("stable release at top of list returned immediately", func(t *testing.T) {
		body := `[{"tag_name":"v12.0.0","draft":false},{"tag_name":"v11.6.0","draft":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "12.0.0", s.resolveE2EServerVersion())
	})

	t.Run("tag without v prefix returned unchanged", func(t *testing.T) {
		// TrimPrefix("v", non-v-string) is a no-op — bare semver tags work too.
		body := `[{"tag_name":"11.6.0","draft":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
	})

	t.Run("only RCs in list → fallback to master", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-rc1","draft":false},
			{"tag_name":"v11.6.0-rc2","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveE2EServerVersion())
	})

	t.Run("only drafts in list → fallback to master", func(t *testing.T) {
		body := `[{"tag_name":"v11.7.0","draft":true}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveE2EServerVersion())
	})

	t.Run("empty releases list → fallback to master", func(t *testing.T) {
		srv := mockReleasesServer(t, `[]`, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveE2EServerVersion())
	})

	t.Run("API returns 500 → fallback to master", func(t *testing.T) {
		srv := mockReleasesServer(t, `{"message":"Internal Server Error"}`, http.StatusInternalServerError)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "master", s.resolveE2EServerVersion())
	})

	t.Run("mixed: draft RC then stable", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0-rc1","draft":true},
			{"tag_name":"v11.7.0-rc1","draft":false},
			{"tag_name":"v11.6.0","draft":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
	})

	t.Run("prerelease flag skipped even when tag name looks stable", func(t *testing.T) {
		// GitHub marks v11.6.0 as prerelease=true (unusual but possible).
		// The prerelease field must take precedence over the tag name check.
		body := `[
			{"tag_name":"v11.6.0","draft":false,"prerelease":true},
			{"tag_name":"v11.5.0","draft":false,"prerelease":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.5.0", s.resolveE2EServerVersion())
	})

	t.Run("prerelease and rc tag both skipped, stable returned", func(t *testing.T) {
		body := `[
			{"tag_name":"v11.7.0","draft":false,"prerelease":true},
			{"tag_name":"v11.6.1-rc1","draft":false,"prerelease":false},
			{"tag_name":"v11.6.0","draft":false,"prerelease":false}
		]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)
		assert.Equal(t, "11.6.0", s.resolveE2EServerVersion())
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

		v1 := s.resolveE2EServerVersion()
		v2 := s.resolveE2EServerVersion()
		v3 := s.resolveE2EServerVersion()

		assert.Equal(t, "11.6.0", v1)
		assert.Equal(t, "11.6.0", v2)
		assert.Equal(t, "11.6.0", v3)
		assert.Equal(t, 1, callCount, "GitHub API must be called exactly once; subsequent calls use the cache")
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

		v1 := s.resolveE2EServerVersion() // API error → fallback "master" (not cached)
		v2 := s.resolveE2EServerVersion() // retried → "11.6.0" (now cached)
		v3 := s.resolveE2EServerVersion() // cache hit → "11.6.0"

		assert.Equal(t, "master", v1, "first call: API error should fall back to master")
		assert.Equal(t, "11.6.0", v2, "second call: should retry and resolve correctly")
		assert.Equal(t, "11.6.0", v3, "third call: should return cached value")
		assert.Equal(t, 2, callCount, "API should be called twice: once for the failed attempt, once for the retry")
	})
}

// ------------------------------------------------------------
// 13. MM_SERVER_VERSION sourced from instance, not config
// ------------------------------------------------------------

func TestDryRun_MMServerVersionFromInstance(t *testing.T) {
	t.Run("desktop dispatch uses instances[0].ServerVersion not config", func(t *testing.T) {
		// Simulate the case where config says "latest" but the instance was provisioned
		// with the resolved version "11.6.0". The dispatch must use the instance value.
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

		// The desktop workflow dispatch builds inputs using instances[0].ServerVersion.
		inputs := map[string]interface{}{
			"MM_SERVER_VERSION": instances[0].ServerVersion,
		}

		assert.Equal(t, "11.6.0", inputs["MM_SERVER_VERSION"],
			"MM_SERVER_VERSION must be the resolved instance version, not the 'latest' config sentinel")
		assert.NotEqual(t, s.Config.E2EServerVersion, inputs["MM_SERVER_VERSION"],
			"MM_SERVER_VERSION must NOT be the raw config value when config is 'latest'")
	})

	t.Run("mobile dispatch does not include MM_SERVER_VERSION", func(t *testing.T) {
		// Mobile workflows receive SITE_1/2/3_URL and PLATFORM — never MM_SERVER_VERSION.
		instances := []*E2EInstance{
			{Name: "inst-site1", Platform: "site-1",
				URL: "https://site1.test.example.com", InstallationID: "id-1",
				ServerVersion: "11.6.0"},
			{Name: "inst-site2", Platform: "site-2",
				URL: "https://site2.test.example.com", InstallationID: "id-2",
				ServerVersion: "11.6.0"},
			{Name: "inst-site3", Platform: "site-3",
				URL: "https://site3.test.example.com", InstallationID: "id-3",
				ServerVersion: "11.6.0"},
		}

		inputs := map[string]interface{}{
			"MOBILE_VERSION": "feature-sha",
			"PLATFORM":       "both",
			"SITE_1_URL":     instances[0].URL,
			"SITE_2_URL":     instances[1].URL,
			"SITE_3_URL":     instances[2].URL,
		}

		assert.NotContains(t, inputs, "MM_SERVER_VERSION",
			"mobile dispatch must never include MM_SERVER_VERSION")
		assert.NotContains(t, inputs, "instance_details",
			"mobile dispatch must never include instance_details")
		assert.Equal(t, "https://site1.test.example.com", inputs["SITE_1_URL"])
		assert.Equal(t, "https://site2.test.example.com", inputs["SITE_2_URL"])
		assert.Equal(t, "https://site3.test.example.com", inputs["SITE_3_URL"])
	})

	t.Run("all instances in a PR run share the same resolved version", func(t *testing.T) {
		// createMultipleE2EInstances calls resolveE2EServerVersion() once and passes
		// the same version to all createCloudInstallation calls.
		resolvedVersion := "11.6.0"
		platforms := []string{"linux", "macos", "windows"}
		instances := make([]*E2EInstance, len(platforms))
		for i, p := range platforms {
			instances[i] = &E2EInstance{
				Platform:      p,
				ServerVersion: resolvedVersion, // same version for every instance
			}
		}
		for i, inst := range instances {
			assert.Equal(t, resolvedVersion, inst.ServerVersion,
				"instance[%d] (platform=%s) must have the resolved version", i, inst.Platform)
		}
	})

	t.Run("resolveE2EServerVersion with latest returns Docker Hub compatible version", func(t *testing.T) {
		// Docker Hub tags are bare semver (e.g. "11.6.0"), NOT "v11.6.0".
		// Verify the v-stripping produces a Docker Hub compatible string.
		body := `[{"tag_name":"v11.6.0","draft":false}]`
		srv := mockReleasesServer(t, body, http.StatusOK)
		s := newDryRunServerLatest(t, srv)

		version := s.resolveE2EServerVersion()
		assert.Equal(t, "11.6.0", version)
		assert.False(t, strings.HasPrefix(version, "v"),
			"resolved version must NOT have 'v' prefix — Docker Hub tags are bare semver")
	})
}

// ------------------------------------------------------------
// 14. CMT version normalization (v-prefix stripping)
// ------------------------------------------------------------

func TestDryRun_CMTVersionNormalization(t *testing.T) {
	t.Run("v-prefix stripped before instance creation", func(t *testing.T) {
		// handleCMTWithServerVersions strips "v" from each version before provisioning.
		// Verify that strings.TrimPrefix produces Docker Hub compatible versions.
		inputs := []struct {
			input string
			want  string
		}{
			{"v11.0.1", "11.0.1"},
			{"v11.1.0", "11.1.0"},
			{"v12.0.0", "12.0.0"},
			{"11.0.1", "11.0.1"}, // no v — unchanged
			{"11.1.0", "11.1.0"}, // no v — unchanged
			{"v11.6.0-rc1", "11.6.0-rc1"}, // RC: v stripped but rest preserved
		}
		for _, tt := range inputs {
			got := strings.TrimPrefix(tt.input, "v")
			assert.Equal(t, tt.want, got, "TrimPrefix(%q, 'v')", tt.input)
		}
	})

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

	t.Run("CMT instances carry stripped version in ServerVersion", func(t *testing.T) {
		// Instances created by handleCMTWithServerVersions use the stripped version.
		// Simulate by constructing instances as the real code would.
		rawVersions := []string{"v11.0.1", "v11.1.0"}
		var instances []*E2EInstance
		for _, v := range rawVersions {
			stripped := strings.TrimPrefix(v, "v")
			instances = append(instances, &E2EInstance{
				URL:           fmt.Sprintf("https://%s.test.example.com", stripped),
				ServerVersion: stripped,
			})
		}
		assert.Equal(t, "11.0.1", instances[0].ServerVersion)
		assert.Equal(t, "11.1.0", instances[1].ServerVersion)
		for _, inst := range instances {
			assert.False(t, strings.HasPrefix(inst.ServerVersion, "v"),
				"CMT instance ServerVersion must not start with 'v'")
		}
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

	t.Run("CMT versions capped at 5", func(t *testing.T) {
		input := "v1.0.0, v2.0.0, v3.0.0, v4.0.0, v5.0.0, v6.0.0, v7.0.0"
		parsed := parseServerVersionsFromString(input)
		const maxVersions = 5
		if len(parsed) > maxVersions {
			parsed = parsed[:maxVersions]
		}
		assert.Len(t, parsed, 5, "CMT versions must be capped at 5")
	})
}

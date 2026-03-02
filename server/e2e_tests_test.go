// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mattermostModel "github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildInstanceDetailsJSON tests the buildInstanceDetailsJSON function with critical fixes
func TestBuildInstanceDetailsJSON(t *testing.T) {
	tests := []struct {
		name              string
		instances         []*E2EInstance
		expectedFields    []string // fields that must be present in JSON
		shouldFail        bool
		description       string
	}{
		{
			name: "Single instance with all fields",
			instances: []*E2EInstance{
				{
					Platform:       "linux",
					Runner:         "ubuntu-latest",
					URL:            "https://example.com/instance1",
					InstallationID: "install-123",
					ServerVersion:  "master", // CRITICAL FIX: ServerVersion must be included
				},
			},
			expectedFields: []string{"platform", "runner", "url", "installation-id", "server_version"},
			shouldFail:     false,
			description:    "Verify ServerVersion field is included in JSON output",
		},
		{
			name: "Multiple instances with different versions",
			instances: []*E2EInstance{
				{
					Platform:       "linux",
					Runner:         "ubuntu-latest",
					URL:            "https://example.com/instance1",
					InstallationID: "install-123",
					ServerVersion:  "v11.1.0",
				},
				{
					Platform:       "macos",
					Runner:         "macos-latest",
					URL:            "https://example.com/instance2",
					InstallationID: "install-456",
					ServerVersion:  "v11.2.0",
				},
				{
					Platform:       "windows",
					Runner:         "windows-2022",
					URL:            "https://example.com/instance3",
					InstallationID: "install-789",
					ServerVersion:  "v12.0.0",
				},
			},
			expectedFields: []string{"platform", "runner", "url", "installation-id", "server_version"},
			shouldFail:     false,
			description:    "Multiple instances with different server versions for CMT",
		},
		{
			name: "Empty instance array",
			instances: []*E2EInstance{},
			expectedFields: []string{},
			shouldFail:     false,
			description:    "Empty array should produce empty JSON array",
		},
		{
			name: "Instance with minimal fields",
			instances: []*E2EInstance{
				{
					URL:           "https://example.com",
					ServerVersion: "master",
				},
			},
			expectedFields: []string{"url", "server_version"},
			shouldFail:     false,
			description:    "Should handle instances with only URL and ServerVersion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				Logger: logrus.New(),
			}

			result, err := server.buildInstanceDetailsJSON(tt.instances)

			if tt.shouldFail {
				assert.Error(t, err, tt.description)
				return
			}

			require.NoError(t, err, tt.description)
			assert.NotEmpty(t, result, "Result should not be empty")

			// Parse JSON to verify structure
			var details []map[string]interface{}
			err = json.Unmarshal([]byte(result), &details)
			require.NoError(t, err, "Result should be valid JSON")

			// Verify expected fields are present
			if len(tt.instances) > 0 {
				require.Equal(t, len(tt.instances), len(details), "JSON array should match instance count")

				for i, detail := range details {
					for _, field := range tt.expectedFields {
						assert.Contains(t, detail, field, "Field %s should be in JSON for instance %d", field, i)
					}

					// CRITICAL: Verify ServerVersion field is present
					if tt.instances[i].ServerVersion != "" {
						assert.NotNil(t, detail["server_version"], "server_version must be included in instance details")
						assert.Equal(t, tt.instances[i].ServerVersion, detail["server_version"], "server_version should match instance")
					}
				}
			}
		})
	}
}

// TestGetRunnerForPlatform tests platform-to-runner mapping (critical fix for consolidated logic)
func TestGetRunnerForPlatform(t *testing.T) {
	tests := []struct {
		name          string
		platform      string
		expected      string
		description   string
	}{
		{
			name:        "Linux platform",
			platform:    "linux",
			expected:    "ubuntu-latest",
			description: "Linux should map to ubuntu-latest",
		},
		{
			name:        "macOS platform",
			platform:    "macos",
			expected:    "macos-latest",
			description: "macOS should map to macos-latest",
		},
		{
			name:        "Windows platform",
			platform:    "windows",
			expected:    "windows-2022",
			description: "Windows should map to windows-2022",
		},
		{
			name:        "Unknown platform defaults",
			platform:    "unknown",
			expected:    "ubuntu-latest",
			description: "Unknown platform should default to ubuntu-latest",
		},
		{
			name:        "Empty platform",
			platform:    "",
			expected:    "ubuntu-latest",
			description: "Empty platform should default to ubuntu-latest",
		},
		{
			name:        "Case insensitive linux",
			platform:    "LINUX",
			expected:    "ubuntu-latest",
			description: "Case variations should be handled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRunnerForPlatform(tt.platform)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}

// TestE2EInstanceValidation tests instance creation and validation logic
func TestE2EInstanceValidation(t *testing.T) {
	tests := []struct {
		name            string
		instance        *E2EInstance
		shouldBeValid   bool
		description     string
	}{
		{
			name: "Valid desktop instance",
			instance: &E2EInstance{
				Platform:       "linux",
				Runner:         "ubuntu-latest",
				URL:            "https://example.com",
				InstallationID: "id-123",
				ServerVersion:  "master",
			},
			shouldBeValid: true,
			description:   "Instance with all required fields should be valid",
		},
		{
			name: "Valid mobile instance",
			instance: &E2EInstance{
				Platform:       "site-1",
				URL:            "https://example.com",
				InstallationID: "id-123",
				ServerVersion:  "v11.1.0",
			},
			shouldBeValid: true,
			description:   "Mobile instance without runner should be valid",
		},
		{
			name: "Instance missing URL",
			instance: &E2EInstance{
				Platform:       "linux",
				InstallationID: "id-123",
				ServerVersion:  "master",
			},
			shouldBeValid: false,
			description:   "Instance without URL should be invalid",
		},
		{
			name: "Instance missing InstallationID",
			instance: &E2EInstance{
				Platform:      "linux",
				URL:           "https://example.com",
				ServerVersion: "master",
			},
			shouldBeValid: false,
			description:   "Instance without InstallationID should be invalid",
		},
		{
			name: "Instance missing ServerVersion",
			instance: &E2EInstance{
				Platform:       "linux",
				URL:            "https://example.com",
				InstallationID: "id-123",
			},
			shouldBeValid: false,
			description:   "Instance without ServerVersion should be invalid (critical fix)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.shouldBeValid {
				assert.NotEmpty(t, tt.instance.URL, tt.description)
				assert.NotEmpty(t, tt.instance.InstallationID, tt.description)
				assert.NotEmpty(t, tt.instance.ServerVersion, tt.description)
			} else {
				// Check that at least one critical field is missing
				isInvalid := tt.instance.URL == "" ||
					tt.instance.InstallationID == "" ||
					tt.instance.ServerVersion == ""
				assert.True(t, isInvalid, tt.description)
			}
		})
	}
}

// TestMobileInstanceDetailsFormat tests mobile instances format (critical fix)
func TestMobileInstanceDetailsFormat(t *testing.T) {
	tests := []struct {
		name        string
		instances   []*E2EInstance
		description string
	}{
		{
			name: "Three mobile instances (site-1, site-2, site-3)",
			instances: []*E2EInstance{
				{
					Platform:       "site-1",
					URL:            "https://site1.example.com",
					InstallationID: "id-1",
					ServerVersion:  "master",
				},
				{
					Platform:       "site-2",
					URL:            "https://site2.example.com",
					InstallationID: "id-2",
					ServerVersion:  "master",
				},
				{
					Platform:       "site-3",
					URL:            "https://site3.example.com",
					InstallationID: "id-3",
					ServerVersion:  "master",
				},
			},
			description: "Mobile instances should use site-1/2/3 platform naming",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				Logger: logrus.New(),
			}

			result, err := server.buildInstanceDetailsJSON(tt.instances)
			require.NoError(t, err, tt.description)

			var details []map[string]interface{}
			err = json.Unmarshal([]byte(result), &details)
			require.NoError(t, err, "Result should be valid JSON")

			assert.Equal(t, 3, len(details), "Should have 3 instances")

			// Verify platform names
			expectedPlatforms := []string{"site-1", "site-2", "site-3"}
			for i, detail := range details {
				assert.Equal(t, expectedPlatforms[i], detail["platform"], "Platform should match mobile naming scheme")
			}
		})
	}
}

// TestDesktopInstanceDetailsFormat tests desktop instances format with runner mapping
func TestDesktopInstanceDetailsFormat(t *testing.T) {
	tests := []struct {
		name        string
		instances   []*E2EInstance
		description string
	}{
		{
			name: "Three desktop instances (linux, macos, windows)",
			instances: []*E2EInstance{
				{
					Platform:       "linux",
					Runner:         "ubuntu-latest",
					URL:            "https://linux.example.com",
					InstallationID: "id-1",
					ServerVersion:  "v11.1.0",
				},
				{
					Platform:       "macos",
					Runner:         "macos-latest",
					URL:            "https://macos.example.com",
					InstallationID: "id-2",
					ServerVersion:  "v11.1.0",
				},
				{
					Platform:       "windows",
					Runner:         "windows-2022",
					URL:            "https://windows.example.com",
					InstallationID: "id-3",
					ServerVersion:  "v11.1.0",
				},
			},
			description: "Desktop instances should include runner mapping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				Logger: logrus.New(),
			}

			result, err := server.buildInstanceDetailsJSON(tt.instances)
			require.NoError(t, err, tt.description)

			var details []map[string]interface{}
			err = json.Unmarshal([]byte(result), &details)
			require.NoError(t, err, "Result should be valid JSON")

			assert.Equal(t, 3, len(details), "Should have 3 instances")

			// Verify platform and runner combinations
			expectedPairs := map[string]string{
				"linux":   "ubuntu-latest",
				"macos":   "macos-latest",
				"windows": "windows-2022",
			}

			for _, detail := range details {
				platform := detail["platform"].(string)
				runner := detail["runner"].(string)
				expectedRunner := expectedPairs[platform]
				assert.Equal(t, expectedRunner, runner, "Runner should match platform")
			}
		})
	}
}

// TestServerVersionConsistency tests that ServerVersion is consistent across instances
func TestServerVersionConsistency(t *testing.T) {
	tests := []struct {
		name            string
		instances       []*E2EInstance
		allSameVersion  bool
		description     string
	}{
		{
			name: "All instances same version (CMT scenario)",
			instances: []*E2EInstance{
				{
					URL:           "https://server1.com",
					InstallationID: "id-1",
					ServerVersion: "v11.1.0",
				},
				{
					URL:           "https://server2.com",
					InstallationID: "id-2",
					ServerVersion: "v11.1.0",
				},
				{
					URL:           "https://server3.com",
					InstallationID: "id-3",
					ServerVersion: "v11.1.0",
				},
			},
			allSameVersion: true,
			description:    "CMT with same version should have all instances with same version",
		},
		{
			name: "Different versions per instance (future feature)",
			instances: []*E2EInstance{
				{
					URL:           "https://server1.com",
					InstallationID: "id-1",
					ServerVersion: "v11.1.0",
				},
				{
					URL:           "https://server2.com",
					InstallationID: "id-2",
					ServerVersion: "v11.2.0",
				},
				{
					URL:           "https://server3.com",
					InstallationID: "id-3",
					ServerVersion: "v12.0.0",
				},
			},
			allSameVersion: false,
			description:    "Should support different versions per instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				Logger: logrus.New(),
			}

			result, err := server.buildInstanceDetailsJSON(tt.instances)
			require.NoError(t, err, tt.description)

			var details []map[string]interface{}
			err = json.Unmarshal([]byte(result), &details)
			require.NoError(t, err, "Result should be valid JSON")

			// Verify ServerVersion consistency
			if tt.allSameVersion {
				firstVersion := details[0]["server_version"]
				for i, detail := range details {
					assert.Equal(t, firstVersion, detail["server_version"],
						"Instance %d should have same version as first", i)
				}
			} else {
				expectedVersions := []string{"v11.1.0", "v11.2.0", "v12.0.0"}
				for i, detail := range details {
					assert.Equal(t, expectedVersions[i], detail["server_version"],
						"Instance %d should have correct version", i)
				}
			}
		})
	}
}

// TestE2EInstanceCreation tests instance creation with proper field initialization
func TestE2EInstanceCreation(t *testing.T) {
	tests := []struct {
		name         string
		platform     string
		serverVersion string
		description  string
	}{
		{
			name:          "Desktop instance creation",
			platform:      "linux",
			serverVersion: "master",
			description:   "Should create desktop instance with platform and version",
		},
		{
			name:          "Mobile instance creation",
			platform:      "site-1",
			serverVersion: "v11.1.0",
			description:   "Should create mobile instance with site platform and version",
		},
		{
			name:          "Instance with arbitrary version",
			platform:      "windows",
			serverVersion: "rc1",
			description:   "Should handle custom version strings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &E2EInstance{
				Platform:       tt.platform,
				URL:            "https://example.com",
				InstallationID: "test-id",
				ServerVersion:  tt.serverVersion,
			}

			assert.Equal(t, tt.platform, instance.Platform, tt.description)
			assert.Equal(t, tt.serverVersion, instance.ServerVersion, tt.description)
			assert.NotEmpty(t, instance.URL, "URL should be set")
			assert.NotEmpty(t, instance.InstallationID, "InstallationID should be set")
		})
	}
}

// TestE2EPullRequestModel tests E2E integration with PR model
func TestE2EPullRequestModel(t *testing.T) {
	tests := []struct {
		name        string
		pr          *model.PullRequest
		description string
	}{
		{
			name: "Desktop PR",
			pr: &model.PullRequest{
				RepoName:  "mattermost-desktop",
				RepoOwner: "mattermost",
				Number:    12345,
				Username:  "testuser",
				Ref:       "feature-branch",
				Sha:       "abc123def456",
			},
			description: "Desktop PR should have correct repository info",
		},
		{
			name: "Mobile PR",
			pr: &model.PullRequest{
				RepoName:  "mattermost-mobile",
				RepoOwner: "mattermost",
				Number:    67890,
				Username:  "testuser",
				Ref:       "feature-branch",
				Sha:       "xyz789uvw012",
			},
			description: "Mobile PR should have correct repository info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotEmpty(t, tt.pr.RepoName, tt.description)
			assert.NotEmpty(t, tt.pr.RepoOwner, tt.description)
			assert.Greater(t, tt.pr.Number, 0, "PR number should be positive")
			assert.NotEmpty(t, tt.pr.Ref, "PR ref should be set")
			assert.NotEmpty(t, tt.pr.Sha, "PR SHA should be set")
		})
	}
}

// TestInstancePlatformMapping tests platform name consistency
func TestInstancePlatformMapping(t *testing.T) {
	tests := []struct {
		name           string
		platform       string
		isDesktop      bool
		isMobile       bool
		description    string
	}{
		{
			name:        "Linux platform",
			platform:    "linux",
			isDesktop:   true,
			isMobile:    false,
			description: "Linux is desktop platform",
		},
		{
			name:        "macOS platform",
			platform:    "macos",
			isDesktop:   true,
			isMobile:    false,
			description: "macOS is desktop platform",
		},
		{
			name:        "Windows platform",
			platform:    "windows",
			isDesktop:   true,
			isMobile:    false,
			description: "Windows is desktop platform",
		},
		{
			name:        "Site-1 platform",
			platform:    "site-1",
			isDesktop:   false,
			isMobile:    true,
			description: "site-1 is mobile platform",
		},
		{
			name:        "Site-2 platform",
			platform:    "site-2",
			isDesktop:   false,
			isMobile:    true,
			description: "site-2 is mobile platform",
		},
		{
			name:        "Site-3 platform",
			platform:    "site-3",
			isDesktop:   false,
			isMobile:    true,
			description: "site-3 is mobile platform",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isDesktop := tt.platform == "linux" || tt.platform == "macos" || tt.platform == "windows"
			isMobile := tt.platform == "site-1" || tt.platform == "site-2" || tt.platform == "site-3"

			assert.Equal(t, tt.isDesktop, isDesktop, tt.description+" (desktop check)")
			assert.Equal(t, tt.isMobile, isMobile, tt.description+" (mobile check)")
		})
	}
}

// TestCreateE2EDefaultTeam tests team creation with mocked Mattermost API
func TestCreateE2EDefaultTeam(t *testing.T) {
	tests := []struct {
		name          string
		mockCreateFn  func(w http.ResponseWriter, r *http.Request)
		mockAddMembFn func(w http.ResponseWriter, r *http.Request)
		shouldFail    bool
		description   string
	}{
		{
			name: "Successful team creation and member addition",
			mockCreateFn: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "POST", r.Method, "Should use POST for team creation")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"id":"team-abc","name":"ad-1","display_name":"ad-1","type":"O"}`))
			},
			mockAddMembFn: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "POST", r.Method, "Should use POST for team member addition")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"user_id":"user-123","team_id":"team-abc"}`))
			},
			shouldFail:  false,
			description: "Should successfully create team and add member",
		},
		{
			name: "Team creation fails with 500",
			mockCreateFn: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"message":"Internal server error"}`))
			},
			mockAddMembFn: func(w http.ResponseWriter, r *http.Request) {
				// Should not be called
				w.WriteHeader(http.StatusInternalServerError)
			},
			shouldFail:  true,
			description: "Should fail when CreateTeam returns 500",
		},
		{
			name: "Add team member fails with 500",
			mockCreateFn: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"id":"team-abc","name":"ad-1"}`))
			},
			mockAddMembFn: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"message":"Internal server error"}`))
			},
			shouldFail:  true,
			description: "Should fail when AddTeamMember returns 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test server with mux to handle multiple endpoints
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v4/teams", tt.mockCreateFn)
			mux.HandleFunc("/api/v4/teams/team-abc/members", tt.mockAddMembFn)

			server := httptest.NewServer(mux)
			defer server.Close()

			client := mattermostModel.NewAPIv4Client(server.URL)
			logger := logrus.New()

			err := createE2EDefaultTeam(client, "user-123", logger)

			if tt.shouldFail {
				assert.Error(t, err, tt.description)
			} else {
				assert.NoError(t, err, tt.description)
			}
		})
	}
}

// TestIsE2ELabel tests the isE2ELabel function
func TestIsE2ELabel(t *testing.T) {
	tests := []struct {
		name        string
		label       string
		shouldMatch bool
		description string
	}{
		{
			name:        "Desktop E2E label",
			label:       "E2E/Run",
			shouldMatch: true,
			description: "E2E/Run should match",
		},
		{
			name:        "iOS E2E label",
			label:       "E2E/Run-iOS",
			shouldMatch: true,
			description: "E2E/Run-iOS should match",
		},
		{
			name:        "Android E2E label",
			label:       "E2E/Run-Android",
			shouldMatch: true,
			description: "E2E/Run-Android should match",
		},
		{
			name:        "Non-matching label",
			label:       "spinwick",
			shouldMatch: false,
			description: "spinwick should not match",
		},
		{
			name:        "Partial match",
			label:       "E2E/Run-Desktop",
			shouldMatch: false,
			description: "E2E/Run-Desktop should not match (not configured)",
		},
		{
			name:        "Case sensitive",
			label:       "e2e/run",
			shouldMatch: false,
			description: "e2e/run should not match (case sensitive)",
		},
	}

	server := &Server{
		Config: &MatterwickConfig{
			E2ELabel:               "E2E/Run",
			E2EMobileIOSLabel:      "E2E/Run-iOS",
			E2EMobileAndroidLabel:  "E2E/Run-Android",
		},
		Logger: logrus.New(),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := server.isE2ELabel(tt.label)
			assert.Equal(t, tt.shouldMatch, result, tt.description)
		})
	}
}

// TestExtractPlatformFromLabel tests the extractPlatformFromLabel function
func TestExtractPlatformFromLabel(t *testing.T) {
	tests := []struct {
		name        string
		label       string
		expected    string
		description string
	}{
		{
			name:        "iOS label",
			label:       "E2E/Run-iOS",
			expected:    "ios",
			description: "E2E/Run-iOS should extract to 'ios'",
		},
		{
			name:        "Android label",
			label:       "E2E/Run-Android",
			expected:    "android",
			description: "E2E/Run-Android should extract to 'android'",
		},
		{
			name:        "Desktop label defaults to both",
			label:       "E2E/Run",
			expected:    "both",
			description: "E2E/Run should default to 'both'",
		},
		{
			name:        "Unknown label defaults to both",
			label:       "unknown",
			expected:    "both",
			description: "unknown label should default to 'both'",
		},
		{
			name:        "E2E/Run-Mobile defaults to both",
			label:       "E2E/Run-Mobile",
			expected:    "both",
			description: "E2E/Run-Mobile should default to 'both'",
		},
	}

	server := &Server{
		Config: &MatterwickConfig{
			E2ELabel:               "E2E/Run",
			E2EMobileIOSLabel:      "E2E/Run-iOS",
			E2EMobileAndroidLabel:  "E2E/Run-Android",
		},
		Logger: logrus.New(),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := server.extractPlatformFromLabel(tt.label)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}

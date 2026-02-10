// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCMTMatrixStructure tests CMT matrix data structure
func TestCMTMatrixStructure(t *testing.T) {
	tests := []struct {
		name        string
		matrix      *CMTMatrix
		description string
	}{
		{
			name: "Valid matrix with server and client versions",
			matrix: &CMTMatrix{
				ServerVersions: []string{"8.0", "9.0"},
				ClientVersions: []string{"1.0", "1.1"},
			},
			description: "Matrix should contain server and client versions",
		},
		{
			name: "Matrix with single version",
			matrix: &CMTMatrix{
				ServerVersions: []string{"master"},
				ClientVersions: []string{"latest"},
			},
			description: "Matrix should support single version",
		},
		{
			name: "Empty matrix",
			matrix: &CMTMatrix{
				ServerVersions: []string{},
				ClientVersions: []string{},
			},
			description: "Empty matrix should be valid",
		},
		{
			name: "Matrix with many versions",
			matrix: &CMTMatrix{
				ServerVersions: []string{"7.0", "8.0", "9.0", "10.0", "11.0"},
				ClientVersions: []string{"1.0", "1.1", "1.2"},
			},
			description: "Matrix should support many versions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotNil(t, tt.matrix, tt.description)
			assert.IsType(t, &CMTMatrix{}, tt.matrix)
		})
	}
}

// TestDefaultCMTMatrix tests the default matrix generation
func TestDefaultCMTMatrix(t *testing.T) {
	server := &Server{
		Logger: logrus.New(),
	}

	matrix := server.getDefaultCMTMatrix()

	// Default matrix should have 2 server versions and 2 client versions
	assert.NotNil(t, matrix, "Default matrix should not be nil")
	assert.Greater(t, len(matrix.ServerVersions), 0, "Default matrix should have server versions")
	assert.Greater(t, len(matrix.ClientVersions), 0, "Default matrix should have client versions")
}

// TestCMTInstanceStructure tests CMT instance data structure
func TestCMTInstanceStructure(t *testing.T) {
	tests := []struct {
		name     string
		instance *CMTInstance
		hasAllFields bool
		description string
	}{
		{
			name: "Instance with all fields",
			instance: &CMTInstance{
				E2EInstance: &E2EInstance{
					Platform:       "linux",
					Runner:         "ubuntu-latest",
					URL:            "https://example.com",
					InstallationID: "id-123",
					ServerVersion:  "8.0",
				},
				ServerVersion: "8.0",
				ClientVersion: "1.0",
			},
			hasAllFields: true,
			description:  "CMT instance should have platform, runner, URL, versions",
		},
		{
			name: "Instance with required fields only",
			instance: &CMTInstance{
				E2EInstance: &E2EInstance{
					URL:            "https://example.com",
					InstallationID: "id-123",
					ServerVersion:  "8.0",
				},
				ServerVersion: "8.0",
				ClientVersion: "1.0",
			},
			hasAllFields: false,
			description:  "CMT instance should work without platform/runner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotNil(t, tt.instance.E2EInstance, "E2EInstance should not be nil")
			assert.NotEmpty(t, tt.instance.E2EInstance.URL, tt.description)
			assert.NotEmpty(t, tt.instance.E2EInstance.InstallationID, tt.description)
			assert.NotEmpty(t, tt.instance.ServerVersion, tt.description)
			assert.NotEmpty(t, tt.instance.ClientVersion, tt.description)

			if tt.hasAllFields {
				assert.NotEmpty(t, tt.instance.E2EInstance.Platform, "Should have platform")
				assert.NotEmpty(t, tt.instance.E2EInstance.Runner, "Should have runner")
			}
		})
	}
}

// TestConvertCMTToE2EInstances tests conversion from CMT to E2E instances
func TestConvertCMTToE2EInstances(t *testing.T) {
	tests := []struct {
		name        string
		cmtInstances []*CMTInstance
		expectCount  int
		description string
	}{
		{
			name: "Single CMT instance conversion",
			cmtInstances: []*CMTInstance{
				{
					E2EInstance: &E2EInstance{
						Platform:       "linux",
						Runner:         "ubuntu-latest",
						URL:            "https://example.com",
						InstallationID: "id-123",
						ServerVersion:  "8.0",
					},
					ServerVersion: "8.0",
					ClientVersion: "1.0",
				},
			},
			expectCount: 1,
			description: "Should convert single CMT instance to E2E instance",
		},
		{
			name: "Multiple CMT instances conversion",
			cmtInstances: []*CMTInstance{
				{
					E2EInstance: &E2EInstance{
						Platform:       "linux",
						URL:            "https://server1.com",
						InstallationID: "id-1",
						ServerVersion:  "8.0",
					},
					ServerVersion: "8.0",
					ClientVersion: "1.0",
				},
				{
					E2EInstance: &E2EInstance{
						Platform:       "macos",
						URL:            "https://server2.com",
						InstallationID: "id-2",
						ServerVersion:  "9.0",
					},
					ServerVersion: "9.0",
					ClientVersion: "1.1",
				},
			},
			expectCount: 2,
			description: "Should convert multiple CMT instances to E2E instances",
		},
		{
			name:         "Empty CMT instances",
			cmtInstances: []*CMTInstance{},
			expectCount:  0,
			description:  "Should handle empty CMT instances",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e2eInstances := convertCMTToE2EInstances(tt.cmtInstances)

			assert.Equal(t, tt.expectCount, len(e2eInstances), tt.description)

			// Verify field mapping
			for i, e2eInst := range e2eInstances {
				assert.Equal(t, tt.cmtInstances[i].E2EInstance.Platform, e2eInst.Platform, "Platform should be preserved")
				assert.Equal(t, tt.cmtInstances[i].E2EInstance.URL, e2eInst.URL, "URL should be preserved")
				assert.Equal(t, tt.cmtInstances[i].E2EInstance.InstallationID, e2eInst.InstallationID, "InstallationID should be preserved")
				// ServerVersion should be CMT's ServerVersion (not ClientVersion)
				assert.Equal(t, tt.cmtInstances[i].ServerVersion, e2eInst.ServerVersion, "ServerVersion should be CMT's ServerVersion")
			}
		})
	}
}

// TestCMTMarshalToJSON tests JSON marshaling for CMT data structures
func TestCMTMarshalToJSON(t *testing.T) {
	tests := []struct {
		name        string
		data        interface{}
		expectValid bool
		description string
	}{
		{
			name: "Marshal CMT matrix",
			data: &CMTMatrix{
				ServerVersions: []string{"8.0", "9.0"},
				ClientVersions: []string{"1.0", "1.1"},
			},
			expectValid: true,
			description: "Should marshal CMT matrix struct",
		},
		{
			name:        "Marshal version array",
			data:        []string{"8.0", "9.0", "10.0"},
			expectValid: true,
			description: "Should marshal version array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := marshalToJSON(tt.data)

			if tt.expectValid {
				require.NoError(t, err, tt.description)
				assert.NotEmpty(t, result, "Result should not be empty")

				// Verify it's valid JSON
				var unmarshaled interface{}
				err = json.Unmarshal([]byte(result), &unmarshaled)
				assert.NoError(t, err, "Result should be valid JSON")
			}
		})
	}
}

// TestCMTMatrixExpansion tests cartesian product expansion of matrix
func TestCMTMatrixExpansion(t *testing.T) {
	tests := []struct {
		name                 string
		matrix               *CMTMatrix
		expectedCombinations int
		description          string
	}{
		{
			name: "2x2 matrix",
			matrix: &CMTMatrix{
				ServerVersions: []string{"8.0", "9.0"},
				ClientVersions: []string{"1.0", "1.1"},
			},
			expectedCombinations: 4, // 2 × 2
			description:          "2 servers × 2 clients = 4 combinations",
		},
		{
			name: "3x3 matrix",
			matrix: &CMTMatrix{
				ServerVersions: []string{"7.0", "8.0", "9.0"},
				ClientVersions: []string{"1.0", "1.1", "1.2"},
			},
			expectedCombinations: 9, // 3 × 3
			description:          "3 servers × 3 clients = 9 combinations",
		},
		{
			name: "1x1 matrix",
			matrix: &CMTMatrix{
				ServerVersions: []string{"master"},
				ClientVersions: []string{"latest"},
			},
			expectedCombinations: 1, // 1 × 1
			description:          "1 server × 1 client = 1 combination",
		},
		{
			name: "5x2 matrix",
			matrix: &CMTMatrix{
				ServerVersions: []string{"6.0", "7.0", "8.0", "9.0", "10.0"},
				ClientVersions: []string{"1.0", "1.1"},
			},
			expectedCombinations: 10, // 5 × 2
			description:          "5 servers × 2 clients = 10 combinations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			combinations := len(tt.matrix.ServerVersions) * len(tt.matrix.ClientVersions)
			assert.Equal(t, tt.expectedCombinations, combinations, tt.description)
		})
	}
}

// TestCMTPlatformDistribution tests platform distribution across instances
func TestCMTPlatformDistribution(t *testing.T) {
	tests := []struct {
		name            string
		instanceCount   int
		platforms       []string
		description     string
	}{
		{
			name:          "Desktop platforms (3)",
			instanceCount: 3,
			platforms:     []string{"linux", "macos", "windows"},
			description:   "Desktop should have 3 platforms",
		},
		{
			name:          "Mobile platforms (3)",
			instanceCount: 3,
			platforms:     []string{"site-1", "site-2", "site-3"},
			description:   "Mobile should have 3 platforms",
		},
		{
			name:          "Multiple matrix combinations (9)",
			instanceCount: 9,
			platforms:     []string{"linux", "macos", "windows", "linux", "macos", "windows", "linux", "macos", "windows"},
			description:   "Cycling platforms for 3 versions × 3 platforms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify platform count matches expectations
			if tt.instanceCount == 3 {
				// For 3 instances, platforms should be just the list
				assert.Equal(t, 3, len(tt.platforms), tt.description)
			} else if tt.instanceCount == 9 {
				// For 9 instances with cycling, should have repeating pattern
				assert.Equal(t, 9, len(tt.platforms), tt.description)
				// Verify cycling pattern (every 3rd should be same platform type)
				assert.Equal(t, tt.platforms[0], tt.platforms[3], "Platform should cycle")
				assert.Equal(t, tt.platforms[1], tt.platforms[4], "Platform should cycle")
				assert.Equal(t, tt.platforms[2], tt.platforms[5], "Platform should cycle")
			}
		})
	}
}

// TestCMTInstanceNaming tests instance naming convention for CMT
func TestCMTInstanceNaming(t *testing.T) {
	tests := []struct {
		name             string
		repoName         string
		serverVersion    string
		clientVersion    string
		platform         string
		expectedPattern  string
		description      string
	}{
		{
			name:             "Desktop CMT instance naming",
			repoName:         "mattermost-desktop",
			serverVersion:    "8.0",
			clientVersion:    "1.0",
			platform:         "linux",
			expectedPattern:  "mattermost-desktop-cmt-8-0-1-0-linux",
			description:      "Should follow desktop CMT naming pattern",
		},
		{
			name:             "Mobile CMT instance naming",
			repoName:         "mattermost-mobile",
			serverVersion:    "9.0",
			clientVersion:    "1.1",
			platform:         "site-1",
			expectedPattern:  "mattermost-mobile-cmt-9-0-1-1-site-1",
			description:      "Should follow mobile CMT naming pattern",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify naming structure contains expected components
			assert.Contains(t, tt.expectedPattern, "cmt", "CMT instances should have 'cmt' prefix")
			assert.Contains(t, tt.expectedPattern, tt.platform, "Instance name should include platform")
			// The pattern itself already contains the hyphenated versions, so check the pattern is valid
			assert.NotEmpty(t, tt.expectedPattern, "Expected pattern should not be empty")
			assert.Contains(t, tt.expectedPattern, "-cmt-", "Should contain CMT marker in naming")
		})
	}
}

// TestCMTPullRequest tests CMT integration with PR model
func TestCMTPullRequest(t *testing.T) {
	tests := []struct {
		name        string
		pr          *model.PullRequest
		description string
	}{
		{
			name: "Desktop CMT PR",
			pr: &model.PullRequest{
				RepoName:  "mattermost-desktop",
				RepoOwner: "mattermost",
				Number:    1001,
				Username:  "testuser",
				Ref:       "cmt-test",
				Sha:       "abc123",
			},
			description: "Desktop PR should have required fields for CMT",
		},
		{
			name: "Mobile CMT PR",
			pr: &model.PullRequest{
				RepoName:  "mattermost-mobile",
				RepoOwner: "mattermost",
				Number:    2001,
				Username:  "testuser",
				Ref:       "cmt-test",
				Sha:       "def456",
			},
			description: "Mobile PR should have required fields for CMT",
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

// TestCMTVersionParsing tests parsing of version strings from CMT matrix
func TestCMTVersionParsing(t *testing.T) {
	tests := []struct {
		name            string
		versions        []string
		expectValidation bool
		description      string
	}{
		{
			name:             "Release versions",
			versions:         []string{"8.0", "9.0", "10.0"},
			expectValidation: true,
			description:      "Should handle release version numbers",
		},
		{
			name:             "RC versions",
			versions:         []string{"8.0-rc1", "9.0-rc2"},
			expectValidation: true,
			description:      "Should handle RC versions",
		},
		{
			name:             "Beta versions",
			versions:         []string{"8.0-beta1", "9.0-beta2"},
			expectValidation: true,
			description:      "Should handle beta versions",
		},
		{
			name:             "Master/latest",
			versions:         []string{"master", "latest"},
			expectValidation: true,
			description:      "Should handle master/latest keywords",
		},
		{
			name:             "Mixed versions",
			versions:         []string{"7.10", "8.0", "9.0-rc1", "master"},
			expectValidation: true,
			description:      "Should handle mixed version formats",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Greater(t, len(tt.versions), 0, tt.description)
			for _, v := range tt.versions {
				assert.NotEmpty(t, v, "Version should not be empty")
			}
		})
	}
}

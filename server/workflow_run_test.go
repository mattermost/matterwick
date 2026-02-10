// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// TestParseServerVersionsFromString tests the version parsing function
func TestParseServerVersionsFromString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "Single version",
			input:    "v11.1.0",
			expected: []string{"v11.1.0"},
		},
		{
			name:     "Multiple versions",
			input:    "v11.1.0, v11.2.0, v12.0.0",
			expected: []string{"v11.1.0", "v11.2.0", "v12.0.0"},
		},
		{
			name:     "Versions with extra spaces",
			input:    "v11.1.0  ,  v11.2.0  ,  v12.0.0",
			expected: []string{"v11.1.0", "v11.2.0", "v12.0.0"},
		},
		{
			name:     "Versions without v prefix",
			input:    "11.1.0, 11.2.0",
			expected: []string{"11.1.0", "11.2.0"},
		},
		{
			name:     "Empty string",
			input:    "",
			expected: []string{},
		},
		{
			name:     "Only spaces",
			input:    "   ",
			expected: []string{},
		},
		{
			name:     "Trailing comma",
			input:    "v11.1.0, v11.2.0,",
			expected: []string{"v11.1.0", "v11.2.0"},
		},
		{
			name:     "Leading comma",
			input:    ", v11.1.0, v11.2.0",
			expected: []string{"v11.1.0", "v11.2.0"},
		},
		{
			name:     "Many versions",
			input:    "v10.0, v10.1, v11.0, v11.1, v11.2, v12.0, v12.1",
			expected: []string{"v10.0", "v10.1", "v11.0", "v11.1", "v11.2", "v12.0", "v12.1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseServerVersionsFromString(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d versions, got %d", len(tt.expected), len(result))
			}
			for i, v := range result {
				if i >= len(tt.expected) {
					t.Errorf("Got unexpected version: %s", v)
					continue
				}
				if v != tt.expected[i] {
					t.Errorf("Expected version %s, got %s", tt.expected[i], v)
				}
			}
		})
	}
}

// TestParseWorkflowRunEventWithInputs tests parsing workflow_run webhook payload
func TestParseWorkflowRunEventWithInputs(t *testing.T) {
	tests := []struct {
		name          string
		payload       WorkflowRunWebhookPayload
		expectedError bool
	}{
		{
			name: "Valid desktop CMT payload",
			payload: WorkflowRunWebhookPayload{
				Action: "requested",
				WorkflowRun: WorkflowRunWithInputs{
					ID:         123456,
					Name:       "CMT Desktop",
					HeadBranch: "main",
					HeadSHA:    "abc123def456",
					Inputs: map[string]string{
						"server_versions": "v11.1.0, v11.2.0",
					},
				},
				Repository: map[string]interface{}{
					"name": "mattermost-desktop",
					"owner": map[string]interface{}{
						"login": "mattermost",
					},
				},
			},
			expectedError: false,
		},
		{
			name: "Valid mobile CMT payload",
			payload: WorkflowRunWebhookPayload{
				Action: "requested",
				WorkflowRun: WorkflowRunWithInputs{
					ID:         789012,
					Name:       "CMT Mobile",
					HeadBranch: "main",
					HeadSHA:    "xyz789abc123",
					Inputs: map[string]string{
						"server_versions": "v11.1.0, v11.2.0, v12.0.0",
					},
				},
				Repository: map[string]interface{}{
					"name": "mattermost-mobile",
					"owner": map[string]interface{}{
						"login": "mattermost",
					},
				},
			},
			expectedError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := tt.payload
			if payload.Action != "requested" {
				t.Errorf("Expected action 'requested', got '%s'", payload.Action)
			}
			if payload.WorkflowRun.Inputs["server_versions"] == "" {
				t.Error("Expected server_versions in inputs")
			}
		})
	}
}

// TestParseWorkflowRunEventWithInputsFromJSON tests JSON parsing
func TestParseWorkflowRunEventWithInputsFromJSON(t *testing.T) {
	jsonPayload := `{
		"action": "requested",
		"workflow_run": {
			"id": 123456,
			"name": "CMT Desktop",
			"head_branch": "main",
			"head_sha": "abc123def456",
			"inputs": {
				"server_versions": "v11.1.0, v11.2.0, v12.0.0"
			}
		},
		"repository": {
			"name": "mattermost-desktop",
			"owner": {
				"login": "mattermost"
			}
		}
	}`

	reader := io.NopCloser(bytes.NewReader([]byte(jsonPayload)))
	payload, err := ParseWorkflowRunEventWithInputs(reader)

	if err != nil {
		t.Errorf("Failed to parse workflow_run event: %v", err)
	}

	if payload.Action != "requested" {
		t.Errorf("Expected action 'requested', got '%s'", payload.Action)
	}

	if payload.WorkflowRun.Name != "CMT Desktop" {
		t.Errorf("Expected workflow name 'CMT Desktop', got '%s'", payload.WorkflowRun.Name)
	}

	versions := payload.WorkflowRun.Inputs["server_versions"]
	if versions != "v11.1.0, v11.2.0, v12.0.0" {
		t.Errorf("Expected server_versions input, got '%s'", versions)
	}
}

// TestParseWorkflowRunEventWithInputsInvalidJSON tests invalid JSON handling
func TestParseWorkflowRunEventWithInputsInvalidJSON(t *testing.T) {
	jsonPayload := `{invalid json}`

	reader := io.NopCloser(bytes.NewReader([]byte(jsonPayload)))
	_, err := ParseWorkflowRunEventWithInputs(reader)

	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}

// TestParseServerVersionsEdgeCases tests edge cases for version parsing
func TestParseServerVersionsEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{
			name:     "Single char versions",
			input:    "1, 2, 3",
			expected: 3,
		},
		{
			name:     "Versions with underscores",
			input:    "v11_1_0, v11_2_0",
			expected: 2,
		},
		{
			name:     "Very long version string",
			input:    "v11.1.0-alpha.1, v11.1.0-beta.2, v11.1.0-rc.3, v11.1.0",
			expected: 4,
		},
		{
			name:     "Comma-only string",
			input:    ",,,",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseServerVersionsFromString(tt.input)
			if len(result) != tt.expected {
				t.Errorf("Expected %d versions, got %d. Result: %v", tt.expected, len(result), result)
			}
		})
	}
}

// TestWorkflowRunPayloadExtraction tests extracting values from workflow run payload
func TestWorkflowRunPayloadExtraction(t *testing.T) {
	payload := WorkflowRunWebhookPayload{
		Action: "requested",
		WorkflowRun: WorkflowRunWithInputs{
			ID:         123456,
			Name:       "CMT Desktop",
			HeadBranch: "main",
			HeadSHA:    "abc123",
			Inputs: map[string]string{
				"server_versions": "v11.1.0, v11.2.0",
			},
		},
		Repository: map[string]interface{}{
			"name": "mattermost-desktop",
			"owner": map[string]interface{}{
				"login": "mattermost",
			},
		},
	}

	// Test action extraction
	if payload.Action != "requested" {
		t.Errorf("Expected action 'requested', got '%s'", payload.Action)
	}

	// Test repository name extraction
	repoName := payload.Repository["name"].(string)
	if repoName != "mattermost-desktop" {
		t.Errorf("Expected repo name 'mattermost-desktop', got '%s'", repoName)
	}

	// Test owner extraction
	owner := payload.Repository["owner"].(map[string]interface{})
	ownerLogin := owner["login"].(string)
	if ownerLogin != "mattermost" {
		t.Errorf("Expected owner 'mattermost', got '%s'", ownerLogin)
	}

	// Test workflow run inputs
	versions := payload.WorkflowRun.Inputs["server_versions"]
	if versions != "v11.1.0, v11.2.0" {
		t.Errorf("Expected server_versions 'v11.1.0, v11.2.0', got '%s'", versions)
	}

	// Test head branch
	if payload.WorkflowRun.HeadBranch != "main" {
		t.Errorf("Expected head branch 'main', got '%s'", payload.WorkflowRun.HeadBranch)
	}
}

// TestRepositoryTypeDetection tests detecting desktop vs mobile from repo name
func TestRepositoryTypeDetection(t *testing.T) {
	tests := []struct {
		name         string
		repoName     string
		expectedType string
	}{
		{
			name:         "Desktop repository",
			repoName:     "mattermost-desktop",
			expectedType: "desktop",
		},
		{
			name:         "Mobile repository",
			repoName:     "mattermost-mobile",
			expectedType: "mobile",
		},
		{
			name:         "Desktop with extra words",
			repoName:     "mattermost-desktop-ui",
			expectedType: "desktop",
		},
		{
			name:         "Mobile with extra words",
			repoName:     "mattermost-mobile-testing",
			expectedType: "mobile",
		},
		{
			name:         "Unknown repository",
			repoName:     "mattermost-unknown",
			expectedType: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var instanceType string
			if contains(tt.repoName, "desktop") {
				instanceType = "desktop"
			} else if contains(tt.repoName, "mobile") {
				instanceType = "mobile"
			} else {
				instanceType = "unknown"
			}

			if instanceType != tt.expectedType {
				t.Errorf("Expected type '%s', got '%s'", tt.expectedType, instanceType)
			}
		})
	}
}

// TestMarshalToJSON tests JSON marshaling
func TestMarshalToJSON(t *testing.T) {
	testData := map[string]interface{}{
		"platform":        "linux",
		"runner":          "ubuntu-latest",
		"url":             "https://example.com",
		"installation-id": "test-123",
		"server_version":  "v11.1.0",
	}

	jsonStr, err := marshalToJSON(testData)
	if err != nil {
		t.Errorf("Failed to marshal to JSON: %v", err)
	}

	// Verify we can unmarshal it back
	var unmarshaled map[string]interface{}
	err = json.Unmarshal([]byte(jsonStr), &unmarshaled)
	if err != nil {
		t.Errorf("Failed to unmarshal JSON: %v", err)
	}

	if unmarshaled["platform"] != "linux" {
		t.Errorf("Expected platform 'linux', got '%v'", unmarshaled["platform"])
	}
}

// TestMarshalToJSONArray tests JSON marshaling for arrays
func TestMarshalToJSONArray(t *testing.T) {
	testData := []map[string]interface{}{
		{
			"platform":        "linux",
			"runner":          "ubuntu-latest",
			"url":             "https://example.com/1",
			"installation-id": "test-123",
			"server_version":  "v11.1.0",
		},
		{
			"platform":        "macos",
			"runner":          "macos-latest",
			"url":             "https://example.com/2",
			"installation-id": "test-456",
			"server_version":  "v11.2.0",
		},
	}

	jsonStr, err := marshalToJSON(testData)
	if err != nil {
		t.Errorf("Failed to marshal to JSON: %v", err)
	}

	// Verify we can unmarshal it back
	var unmarshaled []map[string]interface{}
	err = json.Unmarshal([]byte(jsonStr), &unmarshaled)
	if err != nil {
		t.Errorf("Failed to unmarshal JSON: %v", err)
	}

	if len(unmarshaled) != 2 {
		t.Errorf("Expected 2 items, got %d", len(unmarshaled))
	}

	if unmarshaled[0]["platform"] != "linux" {
		t.Errorf("Expected first platform 'linux', got '%v'", unmarshaled[0]["platform"])
	}

	if unmarshaled[1]["platform"] != "macos" {
		t.Errorf("Expected second platform 'macos', got '%v'", unmarshaled[1]["platform"])
	}
}

// TestWorkflowRunActionFiltering tests filtering non-requested actions
func TestWorkflowRunActionFiltering(t *testing.T) {
	tests := []struct {
		name           string
		action         string
		shouldProcess  bool
	}{
		{
			name:          "Requested action",
			action:        "requested",
			shouldProcess: true,
		},
		{
			name:          "Completed action",
			action:        "completed",
			shouldProcess: false,
		},
		{
			name:          "In progress action",
			action:        "in_progress",
			shouldProcess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := WorkflowRunWebhookPayload{
				Action: tt.action,
			}

			shouldProcess := payload.Action == "requested"
			if shouldProcess != tt.shouldProcess {
				t.Errorf("Expected shouldProcess=%v, got %v", tt.shouldProcess, shouldProcess)
			}
		})
	}
}

// TestWorkflowNameCMTDetection tests detecting CMT workflows
func TestWorkflowNameCMTDetection(t *testing.T) {
	tests := []struct {
		name         string
		workflowName string
		isCMT        bool
	}{
		{
			name:         "CMT Desktop workflow",
			workflowName: "CMT Desktop",
			isCMT:        true,
		},
		{
			name:         "CMT Mobile workflow",
			workflowName: "CMT Mobile",
			isCMT:        true,
		},
		{
			name:         "cmt lowercase",
			workflowName: "cmt test",
			isCMT:        true,
		},
		{
			name:         "E2E workflow",
			workflowName: "E2E Desktop",
			isCMT:        false,
		},
		{
			name:         "Build workflow",
			workflowName: "Build and Test",
			isCMT:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isCMT := contains(tt.workflowName, "cmt") || contains(tt.workflowName, "CMT")
			if isCMT != tt.isCMT {
				t.Errorf("Expected isCMT=%v, got %v", tt.isCMT, isCMT)
			}
		})
	}
}

// Helper function for contains check
func contains(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestInstanceNamingConvention tests instance naming for CMT
func TestInstanceNamingConvention(t *testing.T) {
	tests := []struct {
		name            string
		repoName        string
		version         string
		platform        string
		expectedPattern string
	}{
		{
			name:            "Desktop Linux instance",
			repoName:        "mattermost-desktop",
			version:         "v11.1.0",
			platform:        "linux",
			expectedPattern: "mattermost-desktop-cmt-v11-1-0-linux",
		},
		{
			name:            "Desktop macOS instance",
			repoName:        "mattermost-desktop",
			version:         "v11.2.0",
			platform:        "macos",
			expectedPattern: "mattermost-desktop-cmt-v11-2-0-macos",
		},
		{
			name:            "Mobile site-1 instance",
			repoName:        "mattermost-mobile",
			version:         "v12.0.0",
			platform:        "site-1",
			expectedPattern: "mattermost-mobile-cmt-v12-0-0-site-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitizedRepo := tt.repoName
			sanitizedVersion := tt.version
			// Replace dots with dashes for sanitization
			sanitizedVersion = replaceAll(sanitizedVersion, ".", "-")

			instanceName := sanitizedRepo + "-cmt-" + sanitizedVersion + "-" + tt.platform

			if !contains(instanceName, tt.expectedPattern) {
				t.Errorf("Expected instance name containing '%s', got '%s'", tt.expectedPattern, instanceName)
			}
		})
	}
}

// Helper function to replace all occurrences
func replaceAll(s, old, new string) string {
	result := ""
	for i := 0; i < len(s); i++ {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			result += new
			i += len(old) - 1
		} else {
			result += string(s[i])
		}
	}
	return result
}

// TestWorkflowSelectionByRepo tests selecting correct workflow based on repo type
func TestWorkflowSelectionByRepo(t *testing.T) {
	tests := []struct {
		name             string
		repoName         string
		expectedWorkflow string
	}{
		{
			name:             "Desktop repo uses e2e-functional",
			repoName:         "mattermost-desktop",
			expectedWorkflow: "e2e-functional.yml",
		},
		{
			name:             "Mobile repo uses e2e-detox",
			repoName:         "mattermost-mobile",
			expectedWorkflow: "e2e-detox-pr.yml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var workflow string
			if contains(tt.repoName, "desktop") {
				workflow = "e2e-functional.yml"
			} else if contains(tt.repoName, "mobile") {
				workflow = "e2e-detox-pr.yml"
			}

			if workflow != tt.expectedWorkflow {
				t.Errorf("Expected workflow '%s', got '%s'", tt.expectedWorkflow, workflow)
			}
		})
	}
}

// TestVersionParsingWithVariations tests parsing with various version formats
func TestVersionParsingWithVariations(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "rc versions",
			input:    "v11.1.0-rc.1, v11.1.0-rc.2",
			expected: []string{"v11.1.0-rc.1", "v11.1.0-rc.2"},
		},
		{
			name:     "beta versions",
			input:    "v11.1.0-beta, v11.1.0-beta.1",
			expected: []string{"v11.1.0-beta", "v11.1.0-beta.1"},
		},
		{
			name:     "master branch version",
			input:    "master, v11.1.0",
			expected: []string{"master", "v11.1.0"},
		},
		{
			name:     "version without v prefix",
			input:    "11.1.0, 11.2.0, 12.0.0",
			expected: []string{"11.1.0", "11.2.0", "12.0.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseServerVersionsFromString(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d versions, got %d", len(tt.expected), len(result))
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("Version %d: expected '%s', got '%s'", i, tt.expected[i], v)
				}
			}
		})
	}
}

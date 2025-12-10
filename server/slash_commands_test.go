package server

import (
	"testing"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSpinwickUploadArgs(t *testing.T) {
	logger := logrus.New()
	s := &Server{
		Logger: logger,
	}

	tests := []struct {
		name        string
		args        []string
		expectedDNS string
		expectedErr string
	}{
		{
			name:        "valid dns flag",
			args:        []string{"--dns", "myserver.cloud.mattermost.com"},
			expectedDNS: "myserver.cloud.mattermost.com",
		},
		{
			name:        "dns flag with equals",
			args:        []string{"--dns=myserver.cloud.mattermost.com"},
			expectedDNS: "myserver.cloud.mattermost.com",
		},
		{
			name:        "missing dns flag",
			args:        []string{},
			expectedErr: "--dns flag is required",
		},
		{
			name:        "empty dns value",
			args:        []string{"--dns", ""},
			expectedErr: "--dns flag is required",
		},
		{
			name:        "invalid flag",
			args:        []string{"--invalid-flag"},
			expectedErr: "failed to parse args",
		},
		{
			name:        "help flag",
			args:        []string{"--help"},
			expectedErr: "flag: help requested",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dns, _, err := s.parseSpinwickUploadArgs(tt.args)
			if tt.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectedDNS, dns)
		})
	}
}

func TestHandleSpinWickSlashCommand(t *testing.T) {
	logger := logrus.New()
	s := &Server{
		Logger: logger,
	}

	tests := []struct {
		name          string
		args          []string
		expectedErr   string
		expectedOut   string
		validateCalls func(t *testing.T, createCalled bool, createEnv cloudModel.EnvVarMap, createSize string,
			updateCalled bool, updateEnv cloudModel.EnvVarMap,
			deleteCalled bool, uploadCalled bool, uploadDNS string)
	}{
		{
			name:        "help command",
			args:        []string{},
			expectedOut: spinwickSlashCommandUsageString,
		},
		{
			name: "create command with env vars and size",
			args: []string{"create", "--env", "VAR1=val1,VAR2=val2", "--size", "miniSingleton"},
			validateCalls: func(t *testing.T, createCalled bool, createEnv cloudModel.EnvVarMap, createSize string,
				updateCalled bool, updateEnv cloudModel.EnvVarMap,
				deleteCalled bool, uploadCalled bool, uploadDNS string,
			) {
				assert.True(t, createCalled)
				assert.Equal(t, "miniSingleton", createSize)
				assert.Equal(t, cloudModel.EnvVarMap{
					"VAR1": cloudModel.EnvVar{Value: "val1"},
					"VAR2": cloudModel.EnvVar{Value: "val2"},
				}, createEnv)
				assert.False(t, updateCalled)
				assert.False(t, deleteCalled)
				assert.False(t, uploadCalled)
			},
		},
		{
			name: "update command with env vars",
			args: []string{"update", "--env", "VAR3=val3"},
			validateCalls: func(t *testing.T, createCalled bool, createEnv cloudModel.EnvVarMap, createSize string,
				updateCalled bool, updateEnv cloudModel.EnvVarMap,
				deleteCalled bool, uploadCalled bool, uploadDNS string,
			) {
				assert.False(t, createCalled)
				assert.True(t, updateCalled)
				assert.Equal(t, cloudModel.EnvVarMap{
					"VAR3": cloudModel.EnvVar{Value: "val3"},
				}, updateEnv)
				assert.False(t, deleteCalled)
				assert.False(t, uploadCalled)
			},
		},
		{
			name: "update command with clear env",
			args: []string{"update", "--clear-env", "VAR3"},
			validateCalls: func(t *testing.T, createCalled bool, createEnv cloudModel.EnvVarMap, createSize string,
				updateCalled bool, updateEnv cloudModel.EnvVarMap,
				deleteCalled bool, uploadCalled bool, uploadDNS string,
			) {
				assert.False(t, createCalled)
				assert.True(t, updateCalled)
				assert.Equal(t, cloudModel.EnvVarMap{
					"VAR3": cloudModel.EnvVar{},
				}, updateEnv)
				assert.False(t, deleteCalled)
				assert.False(t, uploadCalled)
			},
		},
		{
			name: "delete command",
			args: []string{"delete"},
			validateCalls: func(t *testing.T, createCalled bool, createEnv cloudModel.EnvVarMap, createSize string,
				updateCalled bool, updateEnv cloudModel.EnvVarMap,
				deleteCalled bool, uploadCalled bool, uploadDNS string,
			) {
				assert.False(t, createCalled)
				assert.False(t, updateCalled)
				assert.True(t, deleteCalled)
				assert.False(t, uploadCalled)
			},
		},
		{
			name: "upload command with dns",
			args: []string{"upload", "--dns", "myserver.cloud.mattermost.com"},
			validateCalls: func(t *testing.T, createCalled bool, createEnv cloudModel.EnvVarMap, createSize string,
				updateCalled bool, updateEnv cloudModel.EnvVarMap,
				deleteCalled bool, uploadCalled bool, uploadDNS string,
			) {
				assert.False(t, createCalled)
				assert.False(t, updateCalled)
				assert.False(t, deleteCalled)
				assert.True(t, uploadCalled)
				assert.Equal(t, "myserver.cloud.mattermost.com", uploadDNS)
			},
		},
		{
			name:        "upload command missing dns",
			args:        []string{"upload"},
			expectedErr: "--dns flag is required",
		},
		{
			name:        "invalid command",
			args:        []string{"invalid"},
			expectedErr: `invalid command "invalid"`,
		},
		{
			name:        "invalid flag",
			args:        []string{"create", "--invalid-flag"},
			expectedErr: "failed to parse spinwick command args: failed to parse args: flag provided but not defined: -invalid-flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var createCalled bool
			var createEnv cloudModel.EnvVarMap
			var createSize string
			var updateCalled bool
			var updateEnv cloudModel.EnvVarMap
			var deleteCalled bool
			var uploadCalled bool
			var uploadDNS string

			handlers := spinWickSlashCommandsHandlers{
				createHandler: func(envMap cloudModel.EnvVarMap, size string) {
					createCalled = true
					createEnv = envMap
					createSize = size
				},
				updateHandler: func(envMap cloudModel.EnvVarMap) {
					updateCalled = true
					updateEnv = envMap
				},
				deleteHandler: func() {
					deleteCalled = true
				},
				uploadHandler: func(dns string) {
					uploadCalled = true
					uploadDNS = dns
				},
			}

			output, err := s.handleSpinWickSlashCommand(tt.args, handlers)
			if tt.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectedOut, output)

			if tt.validateCalls != nil {
				tt.validateCalls(t, createCalled, createEnv, createSize, updateCalled, updateEnv, deleteCalled, uploadCalled, uploadDNS)
			}
		})
	}
}

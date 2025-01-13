package server

import (
	"testing"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/stretchr/testify/require"
)

func TestParseEnvVars(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		expected cloudModel.EnvVarMap
		err      string
	}{
		{
			name:  "empty",
			input: "",
			err:   "invalid empty argument",
		},
		{
			name:  "invalid",
			input: "invalid",
			err:   "invalid key/val pair: \"invalid\"",
		},
		{
			name:  "duplicate key",
			input: "VAR1=VAL1,VAR1=VAL2",
			err:   "duplicate key: \"VAR1\"",
		},
		{
			name:  "single key/val",
			input: "VAR1=VAL1",
			expected: cloudModel.EnvVarMap{
				"VAR1": cloudModel.EnvVar{Value: "VAL1"},
			},
		},
		{
			name:  "equal sign in value",
			input: "VAR1=VAL=1",
			expected: cloudModel.EnvVarMap{
				"VAR1": cloudModel.EnvVar{Value: "VAL=1"},
			},
		},
		{
			name:  "multiple key/val",
			input: "VAR1=VAL1,VAR2=VAL2,VAR3=VAL3",
			expected: cloudModel.EnvVarMap{
				"VAR1": cloudModel.EnvVar{Value: "VAL1"},
				"VAR2": cloudModel.EnvVar{Value: "VAL2"},
				"VAR3": cloudModel.EnvVar{Value: "VAL3"},
			},
		},
		{
			name:  "spaced key/val",
			input: "VAR1=VAL1, VAR2=VAL2, VAR3=VAL3",
			expected: cloudModel.EnvVarMap{
				"VAR1": cloudModel.EnvVar{Value: "VAL1"},
				"VAR2": cloudModel.EnvVar{Value: "VAL2"},
				"VAR3": cloudModel.EnvVar{Value: "VAL3"},
			},
		},
		{
			name:  "quoted string",
			input: "'VAR1=VAL1'",
			expected: cloudModel.EnvVarMap{
				"VAR1": cloudModel.EnvVar{Value: "VAL1"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseEnvArg(tc.input)
			if tc.err != "" {
				require.EqualError(t, err, tc.err)
				require.Empty(t, parsed)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expected, parsed)
			}
		})
	}
}

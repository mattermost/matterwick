package server

import (
	"encoding/json"
	"net/url"
	"testing"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/stretchr/testify/require"
)

func TestSplitCommaSeparated(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "single value",
			input:    "value1",
			expected: []string{"value1"},
		},
		{
			name:     "multiple values",
			input:    "value1,value2,value3",
			expected: []string{"value1", "value2", "value3"},
		},
		{
			name:     "with spaces",
			input:    "value1, value2 , value3",
			expected: []string{"value1", "value2", "value3"},
		},
		{
			name:     "single quoted",
			input:    "'value1,value2'",
			expected: []string{"value1", "value2"},
		},
		{
			name:     "double quoted",
			input:    `"value1,value2"`,
			expected: []string{"value1", "value2"},
		},
		{
			name:     "empty values filtered",
			input:    "value1,,value2, ,value3",
			expected: []string{"value1", "value2", "value3"},
		},
		{
			name:     "whitespace only values filtered",
			input:    "value1, ,  ,value2",
			expected: []string{"value1", "value2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := splitCommaSeparated(tc.input)
			require.Equal(t, tc.expected, result)
		})
	}
}

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

func TestGrafanaLogsLink(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		require.Empty(t, grafanaLogsLink("", "loki_uid", "inst1"))
		require.Empty(t, grafanaLogsLink("https://grafana.example.com", "", "inst1"))
		require.Empty(t, grafanaLogsLink("https://grafana.example.com", "loki_uid", ""))
	})

	t.Run("builds explore link", func(t *testing.T) {
		link := grafanaLogsLink("https://grafana.example.com/", "loki_uid", "inst1")

		parsed, err := url.Parse(link)
		require.NoError(t, err)
		require.Equal(t, "grafana.example.com", parsed.Host)
		require.Equal(t, "/explore", parsed.Path)
		require.Equal(t, "1", parsed.Query().Get("orgId"))
		require.Equal(t, "1", parsed.Query().Get("schemaVersion"))

		var panes map[string]struct {
			Datasource string `json:"datasource"`
			Queries    []struct {
				Expr       string            `json:"expr"`
				Datasource map[string]string `json:"datasource"`
			} `json:"queries"`
		}
		require.NoError(t, json.Unmarshal([]byte(parsed.Query().Get("panes")), &panes))
		require.Equal(t, "loki_uid", panes["a"].Datasource)
		require.Len(t, panes["a"].Queries, 1)
		require.Equal(t, `{namespace="inst1"}`, panes["a"].Queries[0].Expr)
		require.Equal(t, map[string]string{"type": "loki", "uid": "loki_uid"}, panes["a"].Queries[0].Datasource)
	})
}

func TestLogsLinkMarkdown(t *testing.T) {
	s := &Server{Config: &MatterwickConfig{}}
	require.Empty(t, s.logsLinkMarkdown("inst1", "Click here"))

	s.Config.GrafanaURL = "https://grafana.example.com"
	s.Config.GrafanaLokiDatasourceUID = "loki_uid"
	require.Contains(t, s.logsLinkMarkdown("inst1", "Click here"), "\n**Logs:** [Click here](https://grafana.example.com/explore?")
}

package server

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestmakeRepeatableSpinwickID(t *testing.T) {
	s := &Server{
		Config: &MatterwickConfig{
			DNSNameTestServer: ".test.mattermost.cloud",
		},
	}

	tests := []struct {
		repoName string
		prNumber int
	}{
		{"mattermost-server", 12345},
		{"mattermost-webapp", 54321},
		{"mattermost-fusion-reactor", 777},
	}

	for _, tc := range tests {
		t.Run(tc.repoName, func(t *testing.T) {
			id := s.makeRepeatableSpinwickID(tc.repoName, tc.prNumber)
			assert.Contains(t, id, tc.repoName)
			assert.Contains(t, id, fmt.Sprintf("%d", tc.prNumber))
		})
	}
}

func TestmakeUniqueSpinWickID(t *testing.T) {
	spinwickLabel := "spinwick"
	spinwickHALabel := "spinwick ha"
	s := &Server{
		Config: &MatterwickConfig{
			SetupSpinWick:     spinwickLabel,
			SetupSpinWickHA:   spinwickHALabel,
			DNSNameTestServer: ".test.mattermost.cloud",
		},
	}
	tests := []struct {
		repoName string
		prNumber int
	}{
		{"mattermost-server", 12345},
		{"mattermost-webapp", 54321},
		{"mattermost-fusion-reactor", 777},
	}

	for _, tc := range tests {
		t.Run(tc.repoName, func(t *testing.T) {
			id := s.makeUniqueSpinWickID(tc.repoName, tc.prNumber)
			assert.Contains(t, id, tc.repoName)
			assert.Contains(t, id, fmt.Sprintf("%d", tc.prNumber))
		})
	}
}
func TestMakeSpinWickIDWithLongRepositoryName(t *testing.T) {
	spinwickLabel := "spinwick"
	spinwickHALabel := "spinwick ha"
	s := &Server{
		Config: &MatterwickConfig{
			SetupSpinWick:     spinwickLabel,
			SetupSpinWickHA:   spinwickHALabel,
			DNSNameTestServer: ".test.mattermost.cloud",
		},
	}
	tests := []struct {
		repoName string
		prNumber int
		result   string
	}{
		{"mattermost-server-webapp-fusion-reactor-test-really-long", 8888, "mattermost-server-webapp-fus-pr-8888-"},
		{"mattermostserverwebappfusionreactortestreallylong", 88, "mattermostserverwebappfusionre-pr-88-"},
	}

	for _, tc := range tests {
		t.Run(tc.repoName, func(t *testing.T) {
			id := s.makeUniqueSpinWickID(tc.repoName, tc.prNumber)
			assert.Contains(t, id, tc.result)
			assert.Contains(t, id, fmt.Sprintf("%d", tc.prNumber))
			assert.Len(t, id, 64-len(s.Config.DNSNameTestServer))
		})
	}
}

func TestIsSpinWickLabel(t *testing.T) {
	spinwickLabel := "spinwick"
	spinwickHALabel := "spinwick ha"
	s := &Server{
		Config: &MatterwickConfig{
			SetupSpinWick:   spinwickLabel,
			SetupSpinWickHA: spinwickHALabel,
		},
	}

	tests := []struct {
		label    string
		expected bool
	}{
		{spinwickLabel, true},
		{spinwickHALabel, true},
		{"not a spinwick label", false},
	}

	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			require.Equal(t, tc.expected, s.isSpinWickLabel(tc.label))
		})
	}
}

func TestIsSpinWickLabelInLabels(t *testing.T) {
	spinwickLabel := "spinwick"
	spinwickHALabel := "spinwick ha"
	s := &Server{
		Config: &MatterwickConfig{
			SetupSpinWick:   spinwickLabel,
			SetupSpinWickHA: spinwickHALabel,
		},
	}

	tests := []struct {
		name     string
		labels   []string
		expected bool
	}{
		{
			"spinwick label",
			[]string{
				spinwickLabel,
			},
			true,
		}, {
			"spinwick and ha label",
			[]string{
				spinwickLabel,
				spinwickHALabel,
			},
			true,
		}, {
			"spinwick label and others",
			[]string{
				spinwickLabel,
				"label1",
				"label2",
			},
			true,
		}, {
			"spinwick label and more others",
			[]string{
				"label0",
				spinwickLabel,
				"label1",
				"label2",
			},
			true,
		}, {
			"spinwick ha label and others",
			[]string{
				spinwickHALabel,
				"label1",
				"label2",
			},
			true,
		}, {
			"not a spinwick label",
			[]string{
				"label1",
				"label2",
			},
			false,
		}, {
			"empty",
			[]string{},
			false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, s.isSpinWickLabelInLabels(tc.labels))
		})
	}
}

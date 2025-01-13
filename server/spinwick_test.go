package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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

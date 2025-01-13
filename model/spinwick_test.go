package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewSpinwick(t *testing.T) {
	repoName := "test-repo"
	prNumber := 123
	baseDomain := "example.com"

	spinwick := NewSpinwick(repoName, prNumber, baseDomain)

	assert.Equal(t, repoName, spinwick.RepoName)
	assert.Equal(t, prNumber, spinwick.PRNumber)
	assert.Equal(t, "test-repo-pr-123", spinwick.RepeatableID)
	assert.Equal(t, 22, len(spinwick.UniqueID)) //  5 char random ID + 16 chars for the rest
}

func TestSpinwick_repeatableID(t *testing.T) {
	spinwick := &Spinwick{
		RepoName: "Test-Repo",
		PRNumber: 456,
	}

	assert.Equal(t, "test-repo-pr-456", spinwick.repeatableID())
}

func TestSpinwick_uniqueID(t *testing.T) {
	t.Run("short base domain", func(t *testing.T) {
		spinwick := &Spinwick{
			RepoName: "mattermost",
			PRNumber: 789,
		}
		baseDomain := "example.com"

		uniqueID := spinwick.uniqueID(baseDomain)

		assert.Equal(t, 23, len(uniqueID)) // 5 char random ID + 16 chars for the rest
		assert.True(t, strings.HasPrefix(uniqueID, "mattermost-pr-789-"))
	})

	t.Run("long base domain", func(t *testing.T) {
		spinwick := &Spinwick{
			RepoName: "mattermost",
			PRNumber: 29790,
		}
		baseDomain := "test.cloud.mattermost.com"

		uniqueID := spinwick.uniqueID(baseDomain)

		assert.LessOrEqual(t, len(uniqueID)+len(baseDomain), 64)
		assert.True(t, strings.HasPrefix(uniqueID, "mattermost-pr-29790-"))
	})

}
func TestSpinwick_DNS(t *testing.T) {
	spinwick := &Spinwick{
		UniqueID: "test-unique-id",
	}
	baseDomain := "example.com"

	assert.Equal(t, "test-unique-id.example.com", spinwick.DNS(baseDomain))
}

func TestSpinwick_URL(t *testing.T) {
	spinwick := &Spinwick{
		UniqueID: "test-unique-id",
	}
	baseDomain := "example.com"

	assert.Equal(t, "https://test-unique-id.example.com", spinwick.URL(baseDomain))
}

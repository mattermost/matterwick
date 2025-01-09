package model

import (
	"fmt"
	"strings"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

type Spinwick struct {
	RepoName     string `json:"repo_name"`
	PRNumber     int    `json:"pr_number"`
	RepeatableID string `json:"repeatable_id"`
	UniqueID     string `json:"unique_id"`
}

func NewSpinwick(repoName string, prNumber int, baseDomain string) *Spinwick {
	spinwick := &Spinwick{
		RepoName: repoName,
		PRNumber: prNumber,
	}

	spinwick.RepeatableID = spinwick.repeatableID()
	spinwick.UniqueID = spinwick.uniqueID(baseDomain)

	return spinwick
}

// Generates an ID based on the PR number and repo name that's repeatable so it can be used for identifying and looking up installations
func (s *Spinwick) uniqueID(baseDomain string) string {
	randomID := cloudModel.NewID()[0:5]
	spinWickID := strings.ToLower(fmt.Sprintf("%s-pr-%d-%s", s.RepoName, s.PRNumber, randomID))
	// DNS names in MM cloud have a character limit. The number of characters in the domain - 64 will be how many we need to trim
	numCharactersToTrim := len(spinWickID+baseDomain) - 64
	if numCharactersToTrim > 0 {
		// Calculate the maximum length for repoName
		maxUniqueIDLength := len(spinWickID) - numCharactersToTrim
		if maxUniqueIDLength < 0 {
			maxUniqueIDLength = 0
		}
		// trim the repoName and reconstruct spinWickID
		spinWickID = strings.ToLower(spinWickID[:maxUniqueIDLength])
	}
	return spinWickID
}

// Generates an ID based on the PR number and repo name, and appends a random string to make it unique
func (s *Spinwick) repeatableID() string {
	return strings.ToLower(fmt.Sprintf("%s-pr-%d", s.RepoName, s.PRNumber))
}

func (s *Spinwick) DNS(baseDomain string) string {
	return fmt.Sprintf("%s.%s", s.UniqueID, baseDomain)
}

func (s *Spinwick) URL(baseDomain string) string {
	return fmt.Sprintf("https://%s", s.DNS(baseDomain))
}

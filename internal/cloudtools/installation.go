package cloudtools

import (
	"fmt"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

// GetInstallationIDFromOwnerID returns the ID of an installation that matches
// a given OwnerID. Multiple matches will return an error. No match will return
// an empty ID and no error.
func GetInstallationIDFromOwnerID(serverURL, awsAPIKey, ownerID string) (string, string, error) {
	headers := map[string]string{
		"x-api-key": awsAPIKey,
	}
	cloudClient := cloudModel.NewClientWithHeaders(serverURL, headers)
	installations, err := cloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{
		OwnerID:                     ownerID,
		Page:                        0,
		PerPage:                     100,
		IncludeGroupConfig:          false,
		IncludeGroupConfigOverrides: false,
		IncludeDeleted:              false,
	})
	if err != nil {
		return "", "", err
	}

	if len(installations) == 0 {
		return "", "", nil
	}
	if len(installations) == 1 {
		return installations[0].ID, installations[0].Image, nil
	}

	return "", "", fmt.Errorf("found %d installations with ownerID %s", len(installations), ownerID)
}

package cloudtools

import (
	cloud "github.com/mattermost/mattermost-cloud/model"
	"github.com/pkg/errors"
)

// GetInstallationIDFromOwnerID returns the installation that matches a given
// OwnerID. Multiple matches will return an error. No match will return
// an empty ID and no error.
func GetInstallationIDFromOwnerID(serverURL, awsAPIKey, ownerID string) (*cloud.InstallationDTO, error) {
	headers := map[string]string{
		"x-api-key": awsAPIKey,
	}
	cloudClient := cloud.NewClientWithHeaders(serverURL, headers)
	installations, err := cloudClient.GetInstallations(&cloud.GetInstallationsRequest{
		OwnerID:                     ownerID,
		Paging:                      cloud.AllPagesNotDeleted(),
		IncludeGroupConfig:          false,
		IncludeGroupConfigOverrides: false,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve installations from provisioner")
	}

	if len(installations) == 0 {
		return nil, nil
	}
	if len(installations) == 1 {
		return installations[0], nil
	}

	return nil, errors.Errorf("found %d installations with ownerID %s", len(installations), ownerID)
}

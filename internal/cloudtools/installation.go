package cloudtools

import (
	cloud "github.com/mattermost/mattermost-cloud/model"
	"github.com/pkg/errors"
)

func isNotDeletedState(state string) bool {
	return state != cloud.InstallationStateDeleted && state != cloud.InstallationStateDeletionPending && state != cloud.InstallationStateDeletionRequested && state != cloud.InstallationStateDeletionFailed
}

// GetInstallationIDFromOwnerID returns the installation that matches a given
// OwnerID. Multiple matches will return an error. No match will return
// an empty ID and no error.
func GetInstallationIDFromOwnerID(client *cloud.Client, serverURL, ownerID string) (*cloud.InstallationDTO, error) {
	installations, err := client.GetInstallations(&cloud.GetInstallationsRequest{
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

	// If there are more than 1 installations, return the first one that isn't deleted
	// This allows support for creating new installations while old ones are sitting in deletion pending.
	for _, installation := range installations {
		if isNotDeletedState(installation.State) {
			return installation, nil
		}
	}

	return nil, errors.Errorf("found %d installations with ownerID %s", len(installations), ownerID)
}

func GetInstallationDNSFromDNSRecords(installation *cloud.InstallationDTO) string {
	for _, dns := range installation.DNSRecords {
		if dns != nil && dns.DeleteAt == 0 {
			return dns.DomainName
		}
	}
	return ""
}

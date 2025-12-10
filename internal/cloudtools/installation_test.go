package cloudtools

import (
	"testing"

	cloud "github.com/mattermost/mattermost-cloud/model"
	"github.com/stretchr/testify/assert"
)

func TestIsNotDeletedState(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		expected bool
	}{
		{
			name:     "stable state",
			state:    cloud.InstallationStateStable,
			expected: true,
		},
		{
			name:     "creation requested",
			state:    cloud.InstallationStateCreationRequested,
			expected: true,
		},
		{
			name:     "deleted state",
			state:    cloud.InstallationStateDeleted,
			expected: false,
		},
		{
			name:     "deletion pending",
			state:    cloud.InstallationStateDeletionPending,
			expected: false,
		},
		{
			name:     "deletion requested",
			state:    cloud.InstallationStateDeletionRequested,
			expected: false,
		},
		{
			name:     "deletion pending requested",
			state:    cloud.InstallationStateDeletionPendingRequested,
			expected: false,
		},
		{
			name:     "deletion pending in progress",
			state:    cloud.InstallationStateDeletionPendingInProgress,
			expected: false,
		},
		{
			name:     "deletion failed",
			state:    cloud.InstallationStateDeletionFailed,
			expected: false,
		},
		{
			name:     "hibernating state",
			state:    cloud.InstallationStateHibernating,
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := isNotDeletedState(tc.state)
			assert.Equal(t, tc.expected, result, "isNotDeletedState(%s) should return %v", tc.state, tc.expected)
		})
	}
}

func TestGetInstallationDNSFromDNSRecords(t *testing.T) {
	t.Run("single active DNS record", func(t *testing.T) {
		installation := &cloud.InstallationDTO{
			DNSRecords: []*cloud.InstallationDNS{
				{
					DomainName: "myserver.cloud.mattermost.com",
					DeleteAt:   0,
				},
			},
		}

		dns := GetInstallationDNSFromDNSRecords(installation)
		assert.Equal(t, "myserver.cloud.mattermost.com", dns)
	})

	t.Run("multiple DNS records with one deleted", func(t *testing.T) {
		installation := &cloud.InstallationDTO{
			DNSRecords: []*cloud.InstallationDNS{
				{
					DomainName: "old.cloud.mattermost.com",
					DeleteAt:   1234567890,
				},
				{
					DomainName: "active.cloud.mattermost.com",
					DeleteAt:   0,
				},
			},
		}

		dns := GetInstallationDNSFromDNSRecords(installation)
		assert.Equal(t, "active.cloud.mattermost.com", dns)
	})

	t.Run("no active DNS records", func(t *testing.T) {
		installation := &cloud.InstallationDTO{
			DNSRecords: []*cloud.InstallationDNS{
				{
					DomainName: "deleted.cloud.mattermost.com",
					DeleteAt:   1234567890,
				},
			},
		}

		dns := GetInstallationDNSFromDNSRecords(installation)
		assert.Equal(t, "", dns)
	})

	t.Run("empty DNS records", func(t *testing.T) {
		installation := &cloud.InstallationDTO{
			DNSRecords: []*cloud.InstallationDNS{},
		}

		dns := GetInstallationDNSFromDNSRecords(installation)
		assert.Equal(t, "", dns)
	})

	t.Run("nil DNS record in slice", func(t *testing.T) {
		installation := &cloud.InstallationDTO{
			DNSRecords: []*cloud.InstallationDNS{
				nil,
				{
					DomainName: "active.cloud.mattermost.com",
					DeleteAt:   0,
				},
			},
		}

		dns := GetInstallationDNSFromDNSRecords(installation)
		assert.Equal(t, "active.cloud.mattermost.com", dns)
	})
}

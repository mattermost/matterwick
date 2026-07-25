// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

// CWS contains all configuration for the Customer Web Server
type CWS struct {
	Database                   string
	CWSSiteURL                 string
	CWSSMTPUsername            string
	CWSSMTPPassword            string
	CWSSMTPServer              string
	CWSSMTPPort                string
	CWSSMTPServerTimeout       string
	CWSSMTPConnectionSecurity  string
	CWSEmailReplyToName        string
	CWSEmailReplyToAddress     string
	CWSEmailBCCAddress         string
	CWSCloudURL                string
	CWSStripeKey               string
	CWSCloudDNSDomain          string
	CWSCloudGroupID            string
	CWSBlapiURL                string
	CWSBlapiToken              string
	CWSLicenseGeneratorURL     string
	CWSLicenseGeneratorKey     string
	CWSDisableRenewalChecks    string
	CWSSplitKey                string
	CWSSplitServerID           string
	CloudDefaultProductID      string
	CloudDefaultTrialProductID string
	DockerHubCredentials       string
	CWSPublicPort              string
	CWSPrivatePort             string
}

// CloudAuth contains all configuration for the Cloud Auth
type CloudAuth struct {
	ClientID      string
	ClientSecret  string
	TokenEndpoint string
}

// MatterwickConfig defines all config for to run the server
type MatterwickConfig struct {
	ListenAddress       string
	MatterWickURL       string
	GithubAccessToken   string
	GitHubTokenReserve  int
	GithubUsername      string
	GitHubWebhookSecret string
	Org                 string
	Username            string

	SetupSpinWick        string
	SetupSpinWickHA      string
	SetupSpinWickWithCWS string
	SpinWickHALicense    string
	ProvisionerServer    string
	AWSAPIKey            string
	DNSNameTestServer    string

	CloudGroupID               string
	SetupSpinmintMessage       string
	SetupSpinmintFailedMessage string
	DestroyedSpinmintMessage   string

	DockerRegistryURL string
	DockerUsername    string
	DockerPassword    string

	MattermostWebhookURL            string
	MattermostWebhookFooter         string
	MattermostCredentialsWebhookURL string
	MattermostCredentialsChannelURL string

	KubeClusterName   string
	KubeClusterRegion string

	LogSettings struct {
		EnableDebug bool
		ConsoleJSON bool
	}

	CWSPublicAPIAddress   string
	CWSInternalAPIAddress string
	CWSAPIKey             string
	CWSUserPassword       string
	CWSSpinwickGroupID    string

	CWS CWS

	CloudAuth CloudAuth

	// PluginRepoToIDMapping maps repository names to plugin IDs for mmctl commands
	// Key: repository name (e.g., "mattermost-plugin-boards")
	// Value: plugin ID to use for mmctl enable command
	PluginRepoToIDMapping map[string]string

	E2ELabel                string
	E2EMobileIOSLabel       string
	E2EMobileAndroidLabel   string
	E2EResetServersLabel    string
	E2EUsername             string
	E2EPassword             string
	E2EServerVersion        string
	E2EAutoTriggerOnMaster  bool
	E2EReleasePatternPrefix string
	E2ETestWorkflowNames    []string // workflow names of the actual test workflows (for completion-based cleanup)
	// E2EInstanceMaxAge is the minimum age (in hours) a non-PR E2E instance must reach
	// before the periodic orphan-cleanup scan will delete it. This prevents the scan
	// from destroying instances that are still being used by a currently-running test.
	// Set to the longest expected E2E run duration plus a small buffer.
	// Default (0): 3 hours.
	E2EInstanceMaxAge int

	// E2EPRInstanceMaxAge is the maximum age (in hours) a PR E2E instance may reach before
	// the periodic cleanup scan deletes it. PR instances are intentionally kept alive between
	// label toggles and across commits so the same servers can be reused for re-runs, so this
	// is much longer than E2EInstanceMaxAge. When such an instance is reaped its in-memory
	// tracking entry is also evicted, so re-applying E2E/Run provisions a fresh set.
	// Default (0): 24 hours.
	E2EPRInstanceMaxAge int

	// CMTTriggerWorkflowName is the workflow name (the "name:" field) of the lightweight
	// CMT trigger workflow in the desktop/mobile repos. Matterwick provisions instances and
	// dispatches compatibility-matrix-testing.yml when it receives a workflow_run "requested"
	// event for this workflow.
	CMTTriggerWorkflowName string

	// CMTTestWorkflowName is the workflow name of the actual CMT test workflow
	// (compatibility-matrix-testing.yml). Used to distinguish CMT completions (cleanup by
	// run ID) from regular E2E completions (cleanup by SHA).
	CMTTestWorkflowName string

	// CMTServerVersions is an OPTIONAL manual override for the CMT version set. When non-empty
	// it is used verbatim (values must be valid Mattermost image tags: full semver, no "v"
	// prefix, e.g. "10.11.0"). When empty (the normal case) matterwick auto-derives the set
	// from Mattermost's GitHub releases via Server.resolveCMTServerVersions (newest ESR
	// lines + latest stable minors + current RC). Leave empty in production.
	CMTServerVersions []string
}

// defaultCMTServerVersions is the fallback CMT version set used only when auto-resolution
// fails (GitHub API error) and no manual override is configured.
var defaultCMTServerVersions = []string{"10.11.22", "11.7.7"}

func findConfigFile(fileName string) string {
	if _, err := os.Stat("/tmp/" + fileName); err == nil {
		fileName, _ = filepath.Abs("/tmp/" + fileName)
	} else if _, err := os.Stat("./config/" + fileName); err == nil {
		fileName, _ = filepath.Abs("./config/" + fileName)
	} else if _, err := os.Stat("../config/" + fileName); err == nil {
		fileName, _ = filepath.Abs("../config/" + fileName)
	} else if _, err := os.Stat(fileName); err == nil {
		fileName, _ = filepath.Abs(fileName)
	}

	return fileName
}

// GetConfig gets the config
func GetConfig(fileName string) (*MatterwickConfig, error) {
	config := &MatterwickConfig{}
	fileName = findConfigFile(fileName)

	file, err := os.Open(fileName)
	if err != nil {
		return config, errors.Wrap(err, "unable to open config file")
	}

	decoder := json.NewDecoder(file)
	err = decoder.Decode(config)
	if err != nil {
		return config, errors.Wrap(err, "unable to decode config file")
	}

	return config, nil
}

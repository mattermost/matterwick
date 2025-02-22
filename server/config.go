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

	MattermostWebhookURL    string
	MattermostWebhookFooter string

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
}

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

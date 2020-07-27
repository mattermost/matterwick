// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

type CWS struct {
	DATABASE                    string
	CWS_PAYMENT_URL             string
	CWS_PAYMENT_TOKEN           string
	CWS_SITEURL                 string
	CWS_SMTP_USERNAME           string
	CWS_SMTP_PASSWORD           string
	CWS_SMTP_SERVER             string
	CWS_SMTP_PORT               string
	CWS_SMTP_SERVERTIMEOUT      string
	CWS_SMTP_CONNECTIONSECURITY string
	CWS_EMAIL_REPLYTONAME       string
	CWS_EMAIL_REPLYTOADDRESS    string
	CWS_EMAIL_BCCADDRESSES      string
	CWS_CLOUD_URL               string
	DOCKER_HUB_CREDENTIALS      string
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

	SetupSpinWick     string
	SetupSpinWickHA   string
	SpinWickHALicense string
	ProvisionerServer string
	AWSAPIKey         string
	DNSNameTestServer string

	CloudGroupID               string
	SetupSpinmintMessage       string
	SetupSpinmintFailedMessage string
	DestroyedSpinmintMessage   string

	DockerRegistryURL string
	DockerUsername    string
	DockerPassword    string

	MattermostWebhookURL    string
	MattermostWebhookFooter string

	LogSettings struct {
		EnableConsole bool
		ConsoleJSON   bool
		ConsoleLevel  string
		EnableFile    bool
		FileJSON      bool
		FileLevel     string
		FileLocation  string
	}

	CWS CWS
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

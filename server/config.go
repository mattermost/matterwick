// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

// LabelResponse contains the label information
type LabelResponse struct {
	Label   string
	Message string
}

// Repository related to the repository
type Repository struct {
	Owner                      string
	Name                       string
	BuildStatusContext         string
	JenkinsServer              string
	InstanceSetupScript        string
	InstanceSetupUpgradeScript string
	JobName                    string
}

// JenkinsCredentials holds the jenkins creds
type JenkinsCredentials struct {
	URL      string
	Username string
	APIToken string
}

// MatterwickConfig defines all config for to run the server
type MatterwickConfig struct {
	ListenAddress               string
	MatterWickURL               string
	GithubAccessToken           string
	GitHubTokenReserve          int
	GithubUsername              string
	GithubAccessTokenCherryPick string
	GitHubWebhookSecret         string
	Org                         string
	Username                    string
	AutoAssignerTeam            string
	AutoAssignerTeamID          int64
	CircleCIToken               string

	DriverName string
	DataSource string

	Repositories []*Repository

	SetupSpinWick     string
	SetupSpinWickHA   string
	SpinWickHALicense string
	ProvisionerServer string
	AWSAPIKey         string
	DNSNameTestServer string
	AWSEmailAccessKey string
	AWSEmailSecretKey string
	AWSEmailEndpoint  string

	SetupSpinmintMessage               string
	SetupSpinmintFailedMessage         string
	DestroyedSpinmintMessage           string
	DestroyedExpirationSpinmintMessage string

	PrLabels []LabelResponse

	JenkinsCredentials map[string]*JenkinsCredentials

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

// GetRepository gets the repo
func GetRepository(repositories []*Repository, owner, name string) (*Repository, bool) {
	for _, repo := range repositories {
		if repo.Owner == owner && repo.Name == name {
			return repo, true
		}
	}

	return nil, false
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/google/go-github/v32/github"
	"github.com/mattermost/mattermost-server/v5/mlog"
	mattermostModel "github.com/mattermost/mattermost-server/v5/model"
	"github.com/pkg/errors"

	"github.com/mattermost/matterwick/model"
)

func (s *Server) uploadPluginIfNecessary(mmClient *mattermostModel.Client4, pr *model.PullRequest) error {
	if !strings.HasPrefix(pr.RepoName, "mattermost-plugin-") || pr.RepoName == "mattermost-plugin-api" {
		return nil
	}

	mlog.Debug("Preparing to upload plugin to the SpinWick server")
	mlog.Debug("Getting CI check information")

	gh := github.NewClient(nil)
	checkName := "ci"
	checks, _, err := gh.Checks.ListCheckRunsForRef(context.Background(), pr.RepoOwner, pr.RepoName, pr.Sha, &github.ListCheckRunsOptions{CheckName: &checkName})
	if err != nil {
		return err
	}

	if len(checks.CheckRuns) == 0 {
		return errors.New("checks.CheckRuns has len 0")
	}

	run := *checks.CheckRuns[0].ExternalID
	workflowData := map[string]string{}
	err = json.Unmarshal([]byte(run), &workflowData)
	if err != nil {
		return err
	}

	mlog.Debug("Getting artifacts URL")

	workflowID := workflowData["workflow-id"]

	jobName := "plugin-ci/build"
	if pr.RepoName == "mattermost-plugin-playbooks" {
		jobName = "build"
	}

	artifactURL, err := getArtifactURLForJob(workflowID, jobName)
	if err != nil {
		return err
	}

	mlog.Debug("Installing plugin artifact from " + artifactURL)

	m, response := mmClient.InstallPluginFromUrl(artifactURL, true)
	if response.Error != nil {
		return response.Error
	}

	if m == nil {
		return errors.New("manifest is nil")
	}

	mlog.Debug("Enabling plugin")

	_, response = mmClient.EnablePlugin(m.Id)
	if response.Error != nil {
		return response.Error
	}

	// do other initialization stuff per https://mattermost.atlassian.net/browse/MM-19546

	return nil
}

func getArtifactURLForJob(workflowID, jobName string) (string, error) {
	u := fmt.Sprintf("https://circleci.com/api/v2/workflow/%v/job", workflowID)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}

	defer res.Body.Close()

	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	type JobsResponse struct {
		Items []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			ProjectSlug string `json:"project_slug"`
			JobNumber   int    `json:"job_number"`
		} `json:"items"`
	}

	jobs := &JobsResponse{}
	err = json.Unmarshal(b, jobs)
	if err != nil {
		return "", err
	}

	num := 0
	slug := ""
	for _, j := range jobs.Items {
		if j.Name == jobName {
			num = j.JobNumber
			slug = j.ProjectSlug
			break
		}
	}

	if slug == "" {
		return "", fmt.Errorf("no job found for name %s", jobName)
	}

	u = fmt.Sprintf("https://circleci.com/api/v2/project/%v/%v/artifacts", slug, num)
	req, err = http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	res, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}

	defer res.Body.Close()

	b, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	type ArtifactsResponse struct {
		Items []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"items"`
	}

	artifacts := &ArtifactsResponse{}
	err = json.Unmarshal(b, artifacts)
	if err != nil {
		return "", err
	}

	for _, a := range artifacts.Items {
		if strings.HasSuffix(a.Path, "tar.gz") {
			return a.URL, nil
		}
	}

	return "", errors.New("couldn't find artifact")
}

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v32/github"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPRAPIURL  = "https://api.github.com/repos/mattermost/mattermost-plugin-weave/pulls/292"
	testPRHTMLURL = "https://github.com/mattermost/mattermost-plugin-weave/pull/292"
)

func newWebhookCaptureServer(t *testing.T) (*httptest.Server, chan string) {
	t.Helper()
	messages := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req WebhookRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		messages <- req.Text
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, messages
}

func TestGetPullRequestFromGithubPopulatesHTMLURL(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"Setup Cloud Test Server"}]`))
	}))
	defer github.Close()
	s := &Server{Config: &MatterwickConfig{}, Logger: logrus.New(), githubAPIBase: github.URL + "/"}

	ghPR := &gh.PullRequest{
		Number:  gh.Int(292),
		State:   gh.String("open"),
		URL:     gh.String(testPRAPIURL),
		HTMLURL: gh.String(testPRHTMLURL),
		User:    &gh.User{Login: gh.String("someone")},
		Head:    &gh.PullRequestBranch{Ref: gh.String("feature"), SHA: gh.String("abc123")},
		Base: &gh.PullRequestBranch{Repo: &gh.Repository{
			Name:  gh.String("mattermost-plugin-weave"),
			Owner: &gh.User{Login: gh.String("mattermost")},
		}},
	}

	pr, err := s.GetPullRequestFromGithub(ghPR)
	require.NoError(t, err)
	assert.Equal(t, testPRAPIURL, pr.URL)
	assert.Equal(t, testPRHTMLURL, pr.HTMLURL)
	assert.Equal(t, []string{"Setup Cloud Test Server"}, pr.Labels)
}

func TestSpinwickSuccessMessageUsesPRHTMLURL(t *testing.T) {
	for _, repoName := range []string{"mattermost-plugin-weave", "mattermost"} {
		t.Run(repoName, func(t *testing.T) {
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("{}"))
			}))
			defer github.Close()
			webhook, messages := newWebhookCaptureServer(t)

			s := &Server{
				Config:        &MatterwickConfig{MattermostCredentialsWebhookURL: webhook.URL},
				Logger:        logrus.New(),
				githubAPIBase: github.URL + "/",
			}
			pr := &model.PullRequest{
				RepoOwner: "mattermost",
				RepoName:  repoName,
				Number:    292,
				URL:       testPRAPIURL,
				HTMLURL:   testPRHTMLURL,
			}
			installation := &cloudModel.InstallationDTO{
				Installation: &cloudModel.Installation{ID: "install-id"},
				DNSRecords:   []*cloudModel.InstallationDNS{{DomainName: "test.example.com"}},
			}

			s.sendSpinwickSuccessToMattermost(pr, installation, "admin-pass", "user-pass", "", s.Logger)

			msg := <-messages
			assert.Contains(t, msg, "**Pull Request:** [#292]("+testPRHTMLURL+")")
			assert.NotContains(t, msg, testPRAPIURL)
		})
	}
}

func TestLogPrettyErrorToMattermostUsesPRHTMLURL(t *testing.T) {
	webhook, messages := newWebhookCaptureServer(t)
	s := &Server{Config: &MatterwickConfig{MattermostWebhookURL: webhook.URL}, Logger: logrus.New()}
	pr := &model.PullRequest{
		RepoOwner: "mattermost",
		RepoName:  "mattermost-plugin-weave",
		Number:    292,
		URL:       testPRAPIURL,
		HTMLURL:   testPRHTMLURL,
	}

	s.logPrettyErrorToMattermost("Spinwick failed", pr, errors.New("boom"), nil, s.Logger)

	msg := <-messages
	assert.Contains(t, msg, "URL: "+testPRHTMLURL)
	assert.NotContains(t, msg, testPRAPIURL)
}

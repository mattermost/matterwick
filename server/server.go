// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/mattermost-server/v5/utils/fileutils"

	"github.com/braintree/manners"
	"github.com/google/go-github/v28/github"
	"github.com/gorilla/mux"
)

// Server is the mattermod server.
type Server struct {
	Config *MatterwickConfig
	Router *mux.Router

	webhookChannelsLock sync.Mutex
	webhookChannels     map[string]chan cloudModel.WebhookPayload

	Builds buildsInterface

	commentLock sync.Mutex

	StartTime time.Time
}

const (
	logFilename = "matterwick.log"

	// buildOverride overrides the buildsInterface of the server for development
	// and testing.
	buildOverride = "MATTERMOD_BUILD_OVERRIDE"
)

// New returns a new server with the desired configuration
func New(config *MatterwickConfig) *Server {
	s := &Server{
		Config:          config,
		Router:          mux.NewRouter(),
		webhookChannels: make(map[string]chan cloudModel.WebhookPayload),
		StartTime:       time.Now(),
	}

	s.Builds = &Builds{}
	if os.Getenv(buildOverride) != "" {
		mlog.Warn("Using mocked build tools")
		s.Builds = &MockedBuilds{
			Version: os.Getenv(buildOverride),
		}
	}

	return s
}

// Start starts a server
func (s *Server) Start() {
	mlog.Info("Starting Mattermod Server")

	rand.Seed(time.Now().Unix())

	s.initializeRouter()

	var handler http.Handler = s.Router
	go func() {
		mlog.Info("Listening on", mlog.String("address", s.Config.ListenAddress))
		err := manners.ListenAndServe(s.Config.ListenAddress, handler)
		if err != nil {
			s.logErrorToMattermost(err.Error())
			mlog.Critical("server_error", mlog.Err(err))
			panic(err.Error())
		}
	}()
}

// Stop stops a server
func (s *Server) Stop() {
	mlog.Info("Stopping Mattermod")
	manners.Close()
}

func (s *Server) initializeRouter() {
	s.Router.HandleFunc("/", s.ping).Methods("GET")
	s.Router.HandleFunc("/github_event", s.githubEvent).Methods("POST")
	s.Router.HandleFunc("/cloud_webhooks", s.handleCloudWebhook).Methods("POST")
	s.Router.HandleFunc("/shrug_wick", s.serveShrugWick).Methods("GET")
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	msg := fmt.Sprintf("{\"MatterWick uptime\": \"%v\"}", time.Since(s.StartTime))
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(msg))
}

func (s *Server) githubEvent(w http.ResponseWriter, r *http.Request) {
	overLimit := s.CheckLimitRateAndAbortRequest()
	if overLimit {
		return
	}

	buf, _ := ioutil.ReadAll(r.Body)

	receivedHash := strings.SplitN(r.Header.Get("X-Hub-Signature"), "=", 2)
	if receivedHash[0] != "sha1" {
		mlog.Error("Invalid webhook hash signature: SHA1")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	err := ValidateSignature(receivedHash, buf, s.Config.GitHubWebhookSecret)
	if err != nil {
		mlog.Error(err.Error())
		w.WriteHeader(http.StatusForbidden)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	switch eventType {
	case "ping":
		pingEvent := PingEventFromJSON(ioutil.NopCloser(bytes.NewBuffer(buf)))
		if pingEvent == nil {
			mlog.Info("ping event failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mlog.Info("ping event", mlog.Int64("HookID", pingEvent.GetHookID()))
	case "pull_request":
		event := PullRequestEventFromJSON(ioutil.NopCloser(bytes.NewBuffer(buf)))
		if event != nil && event.GetNumber() != 0 {
			mlog.Info("pr event", mlog.Int("pr", event.GetNumber()), mlog.String("action", event.GetAction()))
			s.handlePullRequestEvent(event)
		}
	case "issue_comment":
		eventIssueEventComment := IssueCommentEventFromJSON(ioutil.NopCloser(bytes.NewBuffer(buf)))
		if !eventIssueEventComment.GetIssue().IsPullRequest() {
			// if not a pull request dont need to set the status
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if eventIssueEventComment != nil && eventIssueEventComment.GetAction() == "created" {
			if strings.Contains(strings.TrimSpace(eventIssueEventComment.GetComment().GetBody()), "/shrugwick") {
				s.handleShrugWick(eventIssueEventComment)
			}
		}
	default:
		mlog.Info("Other Events")
		w.WriteHeader(http.StatusNotImplemented)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCloudWebhook(w http.ResponseWriter, r *http.Request) {
	payload, err := cloudModel.WebhookPayloadFromReader(r.Body)
	if err != nil {
		mlog.Error("Received webhook event, but couldn't parse the payload")
		return
	}
	defer r.Body.Close()

	payloadClone := *payload
	mlog.Debug("Received cloud webhook payload", mlog.Int("channels", len(s.webhookChannels)), mlog.String("payload", fmt.Sprintf("%+v", payloadClone)))

	s.webhookChannelsLock.Lock()
	for _, channel := range s.webhookChannels {
		go func(ch chan cloudModel.WebhookPayload, p cloudModel.WebhookPayload) {
			select {
			case ch <- p:
			case <-time.After(5 * time.Second):
			}
		}(channel, payloadClone)
	}
	s.webhookChannelsLock.Unlock()
}

func messageByUserContains(comments []*github.IssueComment, username string, text string) bool {
	for _, comment := range comments {
		if *comment.User.Login == username && strings.Contains(*comment.Body, text) {
			return true
		}
	}

	return false
}

// GetLogFileLocation gets the log file locations
func GetLogFileLocation(fileLocation string) string {
	if fileLocation == "" {
		fileLocation, _ = fileutils.FindDir("logs")
	}

	return filepath.Join(fileLocation, logFilename)
}

// SetupLogging sets the logging
func SetupLogging(config *MatterwickConfig) {
	loggingConfig := &mlog.LoggerConfiguration{
		EnableConsole: config.LogSettings.EnableConsole,
		ConsoleJson:   config.LogSettings.ConsoleJSON,
		ConsoleLevel:  strings.ToLower(config.LogSettings.ConsoleLevel),
		EnableFile:    config.LogSettings.EnableFile,
		FileJson:      config.LogSettings.FileJSON,
		FileLevel:     strings.ToLower(config.LogSettings.FileLevel),
		FileLocation:  GetLogFileLocation(config.LogSettings.FileLocation),
	}

	logger := mlog.NewLogger(loggingConfig)
	mlog.RedirectStdLog(logger)
	mlog.InitGlobalLogger(logger)
}

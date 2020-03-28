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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/braintree/manners"
	"github.com/google/go-github/v28/github"
	"github.com/gorilla/mux"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/mattermost-server/v5/utils/fileutils"
	"github.com/mattermost/matterwick/store"
)

// Server is the mattermod server.
type Server struct {
	Config *ServerConfig
	Store  store.Store
	Router *mux.Router

	webhookChannelsLock sync.Mutex
	webhookChannels     map[string]chan cloudModel.WebhookPayload

	Builds buildsInterface

	commentLock sync.Mutex

	StartTime time.Time
}

const (
	INSTANCE_ID_MESSAGE = "Instance ID: "
	LOG_FILENAME        = "matterwick.log"

	// buildOverride overrides the buildsInterface of the server for development
	// and testing.
	buildOverride = "MATTERMOD_BUILD_OVERRIDE"
)

var (
	INSTANCE_ID_PATTERN = regexp.MustCompile(INSTANCE_ID_MESSAGE + "(i-[a-z0-9]+)")
	INSTANCE_ID         = "INSTANCE_ID"
	INTERNAL_IP         = "INTERNAL_IP"
	SPINMINT_LINK       = "SPINMINT_LINK"
)

// New returns a new server with the desired configuration
func New(config *ServerConfig) *Server {
	s := &Server{
		Config:          config,
		Store:           store.NewSqlStore(config.DriverName, config.DataSource),
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
		return
	}

	err := ValidateSignature(receivedHash, buf, s.Config.GitHubWebhookSecret)
	if err != nil {
		mlog.Error(err.Error())
		return
	}

	eventType := os.Getenv("Http_X_Github_Event")

	switch eventType {
	case "ping":
		pingEvent := PingEventFromJson(ioutil.NopCloser(bytes.NewBuffer(buf)))
		if pingEvent != nil {
			mlog.Info("ping event", mlog.Int64("HookID", pingEvent.GetHookID()))
			return
		}
	case "pull_request":
		event := PullRequestEventFromJson(ioutil.NopCloser(bytes.NewBuffer(buf)))
		if event != nil && event.PRNumber != 0 {
			mlog.Info("pr event", mlog.Int("pr", event.PRNumber), mlog.String("action", event.Action))
			s.handlePullRequestEvent(event)
			return
		}
	case "issue_comment":
		eventIssueComment := IssueCommentFromJson(ioutil.NopCloser(bytes.NewBuffer(buf)))
		if eventIssueComment != nil && eventIssueComment.Action == "created" {
			if strings.Contains(strings.TrimSpace(*eventIssueComment.Comment.Body), "/shrugwick") {
				s.handleShrugWick(*eventIssueComment)
			}
			return
		}
	}
}

func (s *Server) handleCloudWebhook(w http.ResponseWriter, r *http.Request) {
	payload, err := cloudModel.WebhookPayloadFromReader(r.Body)
	if err != nil {
		mlog.Error("Received webhook event, but couldn't parse the payload")
		return
	}
	defer r.Body.Close()

	payloadClone := *payload

	s.webhookChannelsLock.Lock()
	mlog.Debug("Received cloud webhook payload", mlog.Int("channels", len(s.webhookChannels)), mlog.String("payload", fmt.Sprintf("%+v", payloadClone)))
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

func GetLogFileLocation(fileLocation string) string {
	if fileLocation == "" {
		fileLocation, _ = fileutils.FindDir("logs")
	}

	return filepath.Join(fileLocation, LOG_FILENAME)
}

func SetupLogging(config *ServerConfig) {
	loggingConfig := &mlog.LoggerConfiguration{
		EnableConsole: config.LogSettings.EnableConsole,
		ConsoleJson:   config.LogSettings.ConsoleJson,
		ConsoleLevel:  strings.ToLower(config.LogSettings.ConsoleLevel),
		EnableFile:    config.LogSettings.EnableFile,
		FileJson:      config.LogSettings.FileJson,
		FileLevel:     strings.ToLower(config.LogSettings.FileLevel),
		FileLocation:  GetLogFileLocation(config.LogSettings.FileLocation),
	}

	logger := mlog.NewLogger(loggingConfig)
	mlog.RedirectStdLog(logger)
	mlog.InitGlobalLogger(logger)
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/braintree/manners"
	"github.com/google/go-github/v32/github"
	"github.com/gorilla/mux"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

// Server is the MatterWick server.
type Server struct {
	Config *MatterwickConfig
	Router *mux.Router

	webhookChannelsLock sync.Mutex
	webhookChannels     map[string]chan cloudModel.WebhookPayload

	Builds buildsInterface

	commentLock sync.Mutex

	StartTime time.Time

	Logger logrus.FieldLogger

	CloudClient *cloudModel.Client

	// envMaps is a map of environment variables for each active installation.
	envMaps     map[string]cloudModel.EnvVarMap
	envMapsLock sync.Mutex
}

const (
	// buildOverride overrides the buildsInterface of the server for development
	// and testing.
	buildOverride = "MATTERWICK_BUILD_OVERRIDE"
)

// New returns a new server with the desired configuration
func New(config *MatterwickConfig) *Server {
	if config.LogSettings.EnableDebug {
		logger.SetLevel(logrus.DebugLevel)
	}
	if config.LogSettings.ConsoleJSON {
		logger.SetFormatter(&logrus.JSONFormatter{})
	}

	var cloudClient *cloudModel.Client
	if os.Getenv("MATTERWICK_LOCAL_TESTING") == "true" {
		cloudClient = cloudModel.NewClient(config.ProvisionerServer)
	} else {
		cloudClient = model.NewCloudClientWithOAuth(config.ProvisionerServer, config.CloudAuth.ClientID, config.CloudAuth.ClientSecret, config.CloudAuth.TokenEndpoint)
	}

	s := &Server{
		Config:          config,
		Router:          mux.NewRouter(),
		webhookChannels: make(map[string]chan cloudModel.WebhookPayload),
		StartTime:       time.Now(),
		Logger:          logger.WithField("instance", cloudModel.NewID()),
		CloudClient:     cloudClient,
		envMaps:         make(map[string]cloudModel.EnvVarMap),
	}

	if !isAwsConfigDefined() {
		s.Logger.Error("Missing environment credentials for AWS Access: AWS_SECRET_ACCESS_KEY, AWS_ACCESS_KEY_ID")
	}

	s.Builds = &Builds{}
	if os.Getenv(buildOverride) != "" {
		s.Logger.Warn("Using mocked build tools")
		s.Builds = &MockedBuilds{
			Version: os.Getenv(buildOverride),
		}
	}

	s.Logger.Info("Config loaded")

	return s
}

// Start starts a server
func (s *Server) Start() {
	s.Logger.Info("Starting MatterWick Server")

	s.initializeRouter()

	var handler http.Handler = s.Router
	go func() {
		s.Logger.WithField("addr", s.Config.ListenAddress).Info("API server listening")
		err := manners.ListenAndServe(s.Config.ListenAddress, handler)
		if err != nil {
			s.logErrorToMattermost(err.Error())
			s.Logger.WithError(err).Panic("server_error")
		}
	}()
}

// Stop stops a server
func (s *Server) Stop() {
	s.Logger.Info("Stopping MatterWick")
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

	buf, _ := io.ReadAll(r.Body)

	receivedHash := strings.SplitN(r.Header.Get("X-Hub-Signature"), "=", 2)
	if receivedHash[0] != "sha1" {
		s.Logger.Error("Invalid webhook hash signature: SHA1")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	err := ValidateSignature(receivedHash, buf, s.Config.GitHubWebhookSecret)
	if err != nil {
		s.Logger.Error(err.Error())
		w.WriteHeader(http.StatusForbidden)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	switch eventType {
	case "ping":
		pingEvent, err := PingEventFromJSON(io.NopCloser(bytes.NewBuffer(buf)))
		if err != nil {
			s.Logger.WithError(err).Error("Failed to parse ping event")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.Logger.WithField("HookID", pingEvent.GetHookID()).Info("ping event")
	case "pull_request":
		event, err := PullRequestEventFromJSON(io.NopCloser(bytes.NewBuffer(buf)))
		if err != nil {
			s.Logger.WithError(err).Error("Failed to parse pull request event")
		}
		// TODO: determine if we need to perform these event number checks or if
		// they can be removed.
		if event != nil && event.GetNumber() != 0 {
			s.Logger.WithFields(logrus.Fields{
				"pr":     event.GetNumber(),
				"action": event.GetAction(),
			}).Info("pr event")
			go s.handlePullRequestEvent(event)
		}
	case "issue_comment":
		eventIssueEventComment, err := IssueCommentEventFromJSON(io.NopCloser(bytes.NewBuffer(buf)))
		if err != nil {
			s.Logger.WithError(err).Error("Failed to parse issue comment event")
		}
		if !eventIssueEventComment.GetIssue().IsPullRequest() {
			// if not a pull request dont need to continue
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if eventIssueEventComment != nil && eventIssueEventComment.GetAction() == "created" {
			msg := strings.TrimSpace(eventIssueEventComment.GetComment().GetBody())
			if strings.HasPrefix(msg, "/") {
				go s.handleSlashCommand(msg, eventIssueEventComment)
			}
		}
	default:
		s.Logger.Info("Other Events")
		w.WriteHeader(http.StatusNotImplemented)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCloudWebhook(w http.ResponseWriter, r *http.Request) {
	payload, err := cloudModel.WebhookPayloadFromReader(r.Body)
	if err != nil {
		s.Logger.WithError(err).Error("Received webhook event, but couldn't parse the payload")
		return
	}
	defer r.Body.Close()

	payloadClone := *payload
	s.Logger.WithFields(logrus.Fields{
		"channels": len(s.webhookChannels),
		"payload":  fmt.Sprintf("%+v", payloadClone),
	}).Debug("Received cloud webhook payload")

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

func (s *Server) getEnvMap(spinwickID string) cloudModel.EnvVarMap {
	s.envMapsLock.Lock()
	defer s.envMapsLock.Unlock()
	return s.envMaps[spinwickID]
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/braintree/manners"
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

	// e2eInstances tracks E2E test instances for cleanup.
	// Key formats: "%s-pr-%d" (PR), "%s-push-%s-%s" (push, ends with SHA),
	// "%s-scheduled-%s" (nightly, ends with SHA), "%s-cmt-%d-%s" (CMT, ends with SHA).
	e2eInstances     map[string][]*E2EInstance
	e2eInstancesLock sync.Mutex
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

	cloudClient := model.NewCloudClient(config.ProvisionerServer, config.CloudAuth.ClientID, config.CloudAuth.ClientSecret, config.CloudAuth.TokenEndpoint, config.AWSAPIKey)

	s := &Server{
		Config:          config,
		Router:          mux.NewRouter(),
		webhookChannels: make(map[string]chan cloudModel.WebhookPayload),
		StartTime:       time.Now(),
		Logger:          logger.WithField("instance", cloudModel.NewID()),
		CloudClient:     cloudClient,
		envMaps:         make(map[string]cloudModel.EnvVarMap),
		e2eInstances:    make(map[string][]*E2EInstance),
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
			s.logErrorToMattermost("%s", err.Error())
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
	s.Router.HandleFunc("/", s.ping).Methods(http.MethodGet)
	s.Router.HandleFunc("/github_event", s.githubEvent).Methods(http.MethodPost)
	s.Router.HandleFunc("/cloud_webhooks", s.handleCloudWebhook).Methods(http.MethodPost)
	s.Router.HandleFunc("/shrug_wick", s.serveShrugWick).Methods(http.MethodGet)
	s.Router.HandleFunc("/cleanup_e2e", s.handleCleanupE2E).Methods(http.MethodPost)
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
			// if not a pull request don't need to continue
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if eventIssueEventComment != nil && eventIssueEventComment.GetAction() == "created" {
			msg := strings.TrimSpace(eventIssueEventComment.GetComment().GetBody())
			if strings.HasPrefix(msg, "/") {
				go s.handleSlashCommand(msg, eventIssueEventComment)
			}
		}
	case "push":
		event, err := PushEventFromJSON(io.NopCloser(bytes.NewBuffer(buf)))
		if err != nil {
			s.Logger.WithError(err).Error("Failed to parse push event")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if event != nil {
			s.Logger.WithField("ref", event.GetRef()).Info("push event")
			go s.handlePushEvent(event)
		}
	case "workflow_run":
		// For workflow_run, we need to parse both the standard event and extract inputs from raw payload
		workflowRunPayload, err := ParseWorkflowRunEventWithInputs(io.NopCloser(bytes.NewBuffer(buf)))
		if err != nil {
			s.Logger.WithError(err).Error("Failed to parse workflow_run event")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if workflowRunPayload != nil {
			s.Logger.WithFields(logrus.Fields{
				"workflow": workflowRunPayload.WorkflowRun.Name,
				"action":   workflowRunPayload.Action,
			}).Info("workflow_run event")
			go s.handleWorkflowRunEventWithInputs(workflowRunPayload)
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

// cleanupE2ERequest is the JSON body for the /cleanup_e2e endpoint.
type cleanupE2ERequest struct {
	Repo  string `json:"repo"`
	RunID int64  `json:"run_id"`
}

// handleCleanupE2E destroys CMT instances when compatibility-matrix-testing.yml completes.
// The workflow sends X-Cleanup-Token header (= E2ECMTCallbackSecret) for auth.
func (s *Server) handleCleanupE2E(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Cleanup-Token")
	if s.Config.E2ECMTCallbackSecret == "" || token != s.Config.E2ECMTCallbackSecret {
		s.Logger.Warn("handleCleanupE2E: invalid or missing cleanup token")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	var req cleanupE2ERequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.Logger.WithError(err).Error("handleCleanupE2E: failed to decode request body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if req.Repo == "" || req.RunID == 0 {
		s.Logger.Error("handleCleanupE2E: missing repo or run_id")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	logger := s.Logger.WithFields(logrus.Fields{
		"repo":   req.Repo,
		"run_id": req.RunID,
	})
	logger.Info("handleCleanupE2E: destroying CMT instances")

	// Remove instances from the tracking map synchronously so callers (and tests)
	// see an immediate consistent state, then destroy the cloud resources in a
	// background goroutine so we can return 202 quickly.
	key := fmt.Sprintf("%s-cmt-%d", req.Repo, req.RunID)
	s.e2eInstancesLock.Lock()
	instances, ok := s.e2eInstances[key]
	if ok {
		delete(s.e2eInstances, key)
	}
	s.e2eInstancesLock.Unlock()

	if ok {
		go s.destroyE2EInstances(instances, logger)
	} else {
		logger.WithField("key", key).Debug("handleCleanupE2E: no CMT instances found for key")
	}

	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) getEnvMap(spinwickID string) cloudModel.EnvVarMap {
	s.envMapsLock.Lock()
	defer s.envMapsLock.Unlock()
	return s.envMaps[spinwickID]
}

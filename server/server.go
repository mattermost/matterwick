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

	// e2eInProgress guards against concurrent handleE2ETestRequest executions for the
	// same PR+platform key (e.g. duplicate webhook deliveries). Only one goroutine per
	// key may run the check-and-create flow at a time; a second arrival while the first
	// is still running is silently dropped.
	e2eInProgress     map[string]bool
	e2eInProgressLock sync.Mutex

	// e2ePRCleanupGeneration tracks how many times handleE2ECleanup has run for
	// each PR key. handleE2ETestRequest captures the counter before provisioning
	// and aborts if it has changed when provisioning completes, preventing stale
	// instances from being stored after a concurrent reset.
	e2ePRCleanupGeneration     map[string]int64
	e2ePRCleanupGenerationLock sync.Mutex

	// stopCh is closed by Stop() to signal long-running background goroutines
	// (e.g. the periodic E2E cleanup ticker) to exit cleanly.
	stopCh chan struct{}

	// githubAPIBase overrides the GitHub API base URL (e.g. "https://api.github.com/").
	// When non-empty (tests only), GitHub clients created inside this server will be
	// redirected to this URL instead of the real GitHub API.
	githubAPIBase string

	// e2eVersionCache holds the last successfully resolved "latest" server version so
	// that back-to-back E2E provisioning requests (e.g. three parallel platform
	// instances) share one GitHub API round-trip instead of each making their own.
	// The cache is intentionally short-lived: new stable releases ship at most once a
	// month, so a 1-hour TTL gives a good hit rate without risking stale data.
	e2eVersionCache     string
	e2eVersionCacheTime time.Time
	e2eVersionCacheLock sync.Mutex
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
		envMaps:                make(map[string]cloudModel.EnvVarMap),
		e2eInstances:           make(map[string][]*E2EInstance),
		e2eInProgress:          make(map[string]bool),
		e2ePRCleanupGeneration: make(map[string]int64),
		stopCh:                 make(chan struct{}),
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

	// Destroy stale non-PR E2E instances left from a previous run immediately on startup,
	// then continue scanning periodically so a mid-run restart doesn't leave orphaned
	// instances alive until the *next* matterwick restart.
	// The scan interval is half the configured max-age so the worst-case orphan lifetime
	// is maxAge + interval ≈ 1.5× maxAge.
	s.cleanupStaleNonPRE2EInstances()
	go func() {
		interval := s.e2eInstanceMaxAge() / 2
		if interval < 30*time.Minute {
			interval = 30 * time.Minute
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanupStaleNonPRE2EInstances()
			case <-s.stopCh:
				return
			}
		}
	}()

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
	close(s.stopCh)
	manners.Close()
}

func (s *Server) initializeRouter() {
	s.Router.HandleFunc("/", s.ping).Methods(http.MethodGet)
	s.Router.HandleFunc("/github_event", s.githubEvent).Methods(http.MethodPost)
	s.Router.HandleFunc("/cloud_webhooks", s.handleCloudWebhook).Methods(http.MethodPost)
	s.Router.HandleFunc("/shrug_wick", s.serveShrugWick).Methods(http.MethodGet)
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

func (s *Server) getEnvMap(spinwickID string) cloudModel.EnvVarMap {
	s.envMapsLock.Lock()
	defer s.envMapsLock.Unlock()
	return s.envMaps[spinwickID]
}

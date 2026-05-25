// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

// CMTDispatchRequest is the JSON body sent by cmt-provisioner.yml when a user
// dispatches CMT against one or more server versions.
//
// GitHub's workflow_run webhook payload does not carry workflow_dispatch inputs
// (verified against the published workflow_run schema), so cmt-provisioner.yml
// POSTs directly to /cmt_dispatch with everything matterwick needs to provision
// instances and dispatch compatibility-matrix-testing.yml.
type CMTDispatchRequest struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	SHA            string `json:"sha"`
	Ref            string `json:"ref"`
	RunID          int64  `json:"run_id"`
	ServerVersions string `json:"server_versions"`
}

// handleCMTDispatch processes a CMT dispatch request from cmt-provisioner.yml.
// It validates the auth token, parses the JSON body, and starts provisioning
// asynchronously so the workflow step returns immediately.
//
// NOTE: this handler does not call CheckLimitRateAndAbortRequest as the
// /github_event handler does. Rate-limit checking made sense for synchronous
// GitHub webhook responses where matterwick might need to make API calls
// before answering. Here, all GitHub API calls happen later in the goroutine,
// so checking now would block the request on a network round-trip to GitHub
// for no benefit, and (more importantly) makes this endpoint untestable in
// the unit-test environment, which has no live token.
func (s *Server) handleCMTDispatch(w http.ResponseWriter, r *http.Request) {
	logger := s.Logger.WithField("endpoint", "/cmt_dispatch")

	// Reject the request if no token is configured rather than running unauthenticated.
	if s.Config.CMTTriggerSecret == "" {
		logger.Error("CMTTriggerSecret not configured; rejecting /cmt_dispatch request")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	provided := r.Header.Get("X-Trigger-Token")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.Config.CMTTriggerSecret)) != 1 {
		logger.Warn("Invalid or missing X-Trigger-Token on /cmt_dispatch")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.WithError(err).Error("Failed to read /cmt_dispatch request body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var req CMTDispatchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		logger.WithError(err).Error("Failed to parse /cmt_dispatch JSON body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if req.Owner == "" || req.Repo == "" || req.SHA == "" || req.Ref == "" || req.RunID == 0 || req.ServerVersions == "" {
		logger.WithFields(logrus.Fields{
			"owner":           req.Owner,
			"repo":            req.Repo,
			"sha_empty":       req.SHA == "",
			"ref_empty":       req.Ref == "",
			"run_id":          req.RunID,
			"versions_empty":  req.ServerVersions == "",
		}).Error("Missing required fields in /cmt_dispatch body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	versions := parseServerVersionsFromString(req.ServerVersions)
	if len(versions) == 0 {
		logger.WithField("input", req.ServerVersions).Error("Failed to parse server_versions")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var instanceType string
	switch {
	case strings.Contains(req.Repo, "desktop"):
		instanceType = "desktop"
	case strings.Contains(req.Repo, "mobile"):
		instanceType = "mobile"
	default:
		logger.WithField("repo", req.Repo).Warn("Repository is neither desktop nor mobile, refusing /cmt_dispatch")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dispatchLogger := logger.WithFields(logrus.Fields{
		"repo":         req.Repo,
		"owner":        req.Owner,
		"sha":          req.SHA,
		"ref":          req.Ref,
		"run_id":       req.RunID,
		"versions":     versions,
		"instanceType": instanceType,
	})
	dispatchLogger.Info("Accepted CMT dispatch request, provisioning asynchronously")

	// Provisioning takes ~30 min; respond 202 now and do the work in a goroutine.
	go s.handleCMTWithServerVersions(req.Owner, req.Repo, instanceType, req.Ref, req.SHA, versions, req.RunID, dispatchLogger)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

// CleanupE2ERequest is the JSON body POSTed by compatibility-matrix-testing.yml
// to /cleanup_e2e after the CMT matrix tests complete, so matterwick can destroy
// the provisioned cloud installations.
type CleanupE2ERequest struct {
	Repo  string `json:"repo"`
	RunID int64  `json:"run_id"`
}

// handleCleanupE2E destroys CMT instances tracked under "{repo}-cmt-{run_id}-*".
// The run_id passed by the workflow is the CMT provisioner's run_id (cmt_run_id),
// which matterwick embeds as the middle component of every CMT tracking key.
//
// NOTE: this handler does not call CheckLimitRateAndAbortRequest. Cleanup hits
// the cloud provisioner API, not GitHub, so GitHub rate limits are not the
// constraint. See handleCMTDispatch for the broader rationale.
func (s *Server) handleCleanupE2E(w http.ResponseWriter, r *http.Request) {
	logger := s.Logger.WithField("endpoint", "/cleanup_e2e")

	if s.Config.CleanupSecret == "" {
		logger.Error("CleanupSecret not configured; rejecting /cleanup_e2e request")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	provided := r.Header.Get("X-Cleanup-Token")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.Config.CleanupSecret)) != 1 {
		logger.Warn("Invalid or missing X-Cleanup-Token on /cleanup_e2e")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.WithError(err).Error("Failed to read /cleanup_e2e body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var req CleanupE2ERequest
	if err := json.Unmarshal(body, &req); err != nil {
		logger.WithError(err).Error("Failed to parse /cleanup_e2e JSON body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if req.Repo == "" || req.RunID == 0 {
		logger.WithFields(logrus.Fields{
			"repo":   req.Repo,
			"run_id": req.RunID,
		}).Error("Missing required fields in /cleanup_e2e body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	cleanupLogger := logger.WithFields(logrus.Fields{
		"repo":   req.Repo,
		"run_id": req.RunID,
		"type":   "cmt_cleanup",
	})

	// Match the CMT tracking-key format: "{repo}-cmt-{run_id}-{sha}".
	keyPrefix := fmt.Sprintf("%s-cmt-%d-", req.Repo, req.RunID)

	s.e2eInstancesLock.Lock()
	var found []*E2EInstance
	var keysToDelete []string
	for key, instances := range s.e2eInstances {
		if strings.HasPrefix(key, keyPrefix) {
			found = append(found, instances...)
			keysToDelete = append(keysToDelete, key)
		}
	}
	for _, k := range keysToDelete {
		delete(s.e2eInstances, k)
	}
	s.e2eInstancesLock.Unlock()

	if len(found) == 0 {
		cleanupLogger.Info("No CMT instances tracked for this run_id; nothing to destroy")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"no_instances"}`))
		return
	}

	cleanupLogger.WithField("instances", len(found)).Info("Destroying CMT instances for completed run")
	go s.destroyE2EInstances(found, cleanupLogger)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"destroying"}`))
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestCleanupE2EEndpoint_Success(t *testing.T) {
	s := &Server{
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
		Config: &MatterwickConfig{
			E2EDesktopRepo:    "desktop",
			ProvisionerServer: "http://fake",
		},
	}
	s.e2eInstances["desktop-cmt-77"] = []*E2EInstance{{Name: "inst", InstallationID: "id"}}

	body, _ := json.Marshal(map[string]interface{}{"repo": "desktop", "run_id": 77})
	req := httptest.NewRequest(http.MethodPost, "/cleanup_e2e", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	s.handleCleanupE2E(rr, req)

	require.Equal(t, http.StatusAccepted, rr.Code)
	s.e2eInstancesLock.Lock()
	_, exists := s.e2eInstances["desktop-cmt-77"]
	s.e2eInstancesLock.Unlock()
	require.False(t, exists)
}

func TestCleanupE2EEndpoint_NoInstances(t *testing.T) {
	s := &Server{
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
		Config:       &MatterwickConfig{},
	}

	body, _ := json.Marshal(map[string]interface{}{"repo": "desktop", "run_id": 99})
	req := httptest.NewRequest(http.MethodPost, "/cleanup_e2e", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	s.handleCleanupE2E(rr, req)

	// Should still return 202 even when no instances found for the key
	require.Equal(t, http.StatusAccepted, rr.Code)
}

func TestCleanupE2EEndpoint_MissingFields(t *testing.T) {
	s := &Server{
		Logger: logrus.New(),
		Config: &MatterwickConfig{},
	}
	body, _ := json.Marshal(map[string]interface{}{"repo": ""})
	req := httptest.NewRequest(http.MethodPost, "/cleanup_e2e", bytes.NewReader(body))

	rr := httptest.NewRecorder()
	s.handleCleanupE2E(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
}

// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WebhookRequest defines the message to send to MM
type WebhookRequest struct {
	Username string `json:"username"`
	Text     string `json:"text"`
}

func (s *Server) sendToWebhook(webhookRequest *WebhookRequest) error {
	b, err := json.Marshal(webhookRequest)
	if err != nil {
		return err
	}

	client := http.Client{}
	request, err := http.NewRequest("POST", s.Config.MattermostWebhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}

	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		return fmt.Errorf("Received non-200 status code when posting to Mattermost: %v", string(contents))
	}

	return nil
}

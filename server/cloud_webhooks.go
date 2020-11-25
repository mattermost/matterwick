package server

import (
	"fmt"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
)

func (s *Server) requestCloudWebhookChannel(id string) (chan cloudModel.WebhookPayload, error) {
	s.webhookChannelsLock.Lock()
	defer s.webhookChannelsLock.Unlock()

	if _, ok := s.webhookChannels[id]; ok {
		return nil, fmt.Errorf("A channel already exists for ID %s", id)
	}
	s.webhookChannels[id] = make(chan cloudModel.WebhookPayload)

	return s.webhookChannels[id], nil
}

func (s *Server) removeCloudWebhookChannel(id string) {
	s.webhookChannelsLock.Lock()
	defer s.webhookChannelsLock.Unlock()
	if _, ok := s.webhookChannels[id]; ok {
		delete(s.webhookChannels, id)
	}
}

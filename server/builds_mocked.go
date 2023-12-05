package server

import (
	"context"

	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"

	"github.com/heroku/docker-registry-client/registry"
)

// MockedBuilds implements buildsInterface but returns hardcoded information.
// This is used for local development and/or testing.
type MockedBuilds struct {
	Version string
}

func (b *MockedBuilds) getInstallationVersion(pr *model.PullRequest) string {
	return b.Version
}

func (b *MockedBuilds) dockerRegistryClient(s *Server) (*registry.Registry, error) {
	return nil, nil
}

func (b *MockedBuilds) waitForImage(ctx context.Context, reg *registry.Registry, desiredTag, imageToCheck string, logger logrus.FieldLogger) error {
	return nil
}

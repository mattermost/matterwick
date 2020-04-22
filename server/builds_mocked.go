package server

import (
	"context"

	"github.com/mattermost/matterwick/model"

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

func (b *MockedBuilds) waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest) (*model.PullRequest, error) {
	return pr, nil
}

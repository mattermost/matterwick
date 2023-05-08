package server

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"

	"github.com/heroku/docker-registry-client/registry"
	"github.com/pkg/errors"
)

// Builds implements buildsInterface for working with external CI/CD systems.
type Builds struct{}

type buildsInterface interface {
	getInstallationVersion(pr *model.PullRequest) string
	dockerRegistryClient(s *Server) (*registry.Registry, error)
	waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest, imageToCheck string, logger logrus.FieldLogger) (*model.PullRequest, error)
}

func (b *Builds) getInstallationVersion(pr *model.PullRequest) string {
	return pr.Sha[0:7]
}

func (b *Builds) dockerRegistryClient(s *Server) (reg *registry.Registry, err error) {
	if _, err = url.ParseRequestURI(s.Config.DockerRegistryURL); err != nil {
		return nil, errors.Wrap(err, "invalid url for docker registry")
	}

	reg, err = registry.New(s.Config.DockerRegistryURL, s.Config.DockerUsername, s.Config.DockerPassword)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to docker registry")
	}

	return reg, nil
}

func (b *Builds) waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest, imageToCheck string, logger logrus.FieldLogger) (*model.PullRequest, error) {
	desiredTag := b.getInstallationVersion(pr)
	logger = logger.WithFields(logrus.Fields{"image": imageToCheck, "tag": desiredTag})

	for {
		select {
		case <-ctx.Done():
			return pr, errors.New("timed out waiting for image to publish")
		case <-time.After(30 * time.Second):
			_, err := reg.ManifestDigest(imageToCheck, desiredTag)
			if err != nil && !strings.Contains(err.Error(), "status=404") {
				return pr, errors.Wrap(err, "unable to fetch tag from docker registry")
			}

			if err == nil {
				logger.Info("Docker tag found!")
				return pr, nil
			}

			logger.Debug("Docker tag for the build not found. Waiting...")
		}
	}
}

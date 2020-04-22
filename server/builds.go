package server

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/matterwick/model"

	"github.com/heroku/docker-registry-client/registry"
	"github.com/pkg/errors"
)

// Builds implements buildsInterface for working with external CI/CD systems.
type Builds struct{}

type buildsInterface interface {
	getInstallationVersion(pr *model.PullRequest) string
	dockerRegistryClient(s *Server) (*registry.Registry, error)
	waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest) (*model.PullRequest, error)
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

func (b *Builds) waitForImage(ctx context.Context, s *Server, reg *registry.Registry, pr *model.PullRequest) (*model.PullRequest, error) {
	for {
		select {
		case <-ctx.Done():
			return pr, errors.New("timed out waiting for image to publish")
		case <-time.After(10 * time.Second):
			desiredTag := b.getInstallationVersion(pr)
			image := "mattermost/mattermost-enterprise-edition"

			_, err := reg.ManifestDigest(image, desiredTag)
			if err != nil && !strings.Contains(err.Error(), "status=404") {
				return pr, errors.Wrap(err, "unable to fetch tag from docker registry")
			}

			if err == nil {
				mlog.Info("docker tag found, image was uploaded", mlog.String("image", image), mlog.String("tag", desiredTag))
				return pr, nil
			}

			mlog.Info("docker tag for the build not found. waiting a bit more...", mlog.String("image", image), mlog.String("tag", desiredTag), mlog.String("repo", pr.RepoName), mlog.Int("number", pr.Number))
		}
	}
}

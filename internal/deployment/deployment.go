package deployment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/config"
	"github.com/google/go-containerregistry/pkg/crane"
	conname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/moby/buildkit/client"
	"golang.org/x/sync/errgroup"
)

var ErrNotFound = errors.New("deployment not found")

type Manager struct {
	logger   *slog.Logger
	cfg      config.Config
	clusters *cluster.Manager
}

func NewManager(logger *slog.Logger, cfg config.Config, clusters *cluster.Manager) *Manager {
	return &Manager{
		logger:   logger,
		cfg:      cfg,
		clusters: clusters,
	}
}

func (m *Manager) Deploy(ctx context.Context, cluster string, name string) error {
	if cluster == "" {
		cluster = m.cfg.DefaultCluster
	}

	provider, err := m.clusters.Provider(cluster)
	if err != nil {
		return err
	}

	b, err := NewBuilder(ctx, m.logger, provider.BuildKitConfig())
	if err != nil {
		return err
	}

	var deployment config.Deployment

	for _, d := range m.cfg.Deployments {
		if d.Name != name {
			continue
		}

		deployment = d
	}

	if deployment == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}

	m.logger.Info("Deploying", "name", deployment.Name)

	regTrans, regAuth, err := provider.RegistryConn(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster registry: %w", err)
	}

	if len(deployment.Images) > 0 {
		m.logger.Info("Building images")

		for _, image := range deployment.Images {
			m.logger.Info("Building image", "image", image.Image)

			statusChan := make(chan *client.SolveStatus)

			errgrp, gctx := errgroup.WithContext(ctx)

			var artifact *Artifact

			errgrp.Go(func() error {
				var err error

				artifact, err = b.Build(gctx, image, "./", statusChan)

				return err
			})

			errgrp.Go(func() error {
				// don't use gctx here

				return DisplayProgress(ctx, statusChan)
			})

			if err := errgrp.Wait(); err != nil {
				if artifact != nil {
					artifact.Delete()
				}

				return err
			}

			m.logger.Info("Pushing image", "image", image.Image)

			img, err := crane.Load(artifact.File.Name())
			if err != nil {
				artifact.Delete()

				return fmt.Errorf("failed to load image: %w", err)
			}

			tag, err := conname.NewTag(image.Image, conname.Insecure)
			if err != nil {
				artifact.Delete()

				return fmt.Errorf("failed to create tag: %w", err)
			}

			if err := remote.Push(
				tag,
				img,
				remote.WithContext(ctx),
				remote.WithTransport(regTrans),
				remote.WithAuth(regAuth),
			); err != nil {
				artifact.Delete()

				return fmt.Errorf("pushing artifact failed: %w", err)
			}

			artifact.Delete()
		}
	}

	m.logger.Info("Building manifests")

	return nil
}

package cluster

import (
	"context"
	"errors"
	"fmt"
	"github.com/csnewman/localflux/internal/config"
	"log/slog"
)

var (
	ErrNoDefault     = errors.New("no default cluster set")
	ErrNotDefined    = errors.New("cluster not defined in config")
	ErrAlreadyExists = errors.New("cluster already exists")
)

type Manager struct {
	logger *slog.Logger
	cfg    config.Config
}

func NewManager(logger *slog.Logger, cfg config.Config) *Manager {
	return &Manager{
		logger: logger,
		cfg:    cfg,
	}
}

func (m *Manager) Start(name string) error {
	if name == "" {
		name = m.cfg.DefaultCluster
	}

	if name == "" {
		return ErrNoDefault
	}

	cfg := m.getConfig(name)
	if cfg == nil {
		return fmt.Errorf("%w: %s", ErrNotDefined, name)
	}

	m.logger.Info("Starting cluster", "name", name)

	ctx := context.Background()

	mk := NewMinikube(m.logger)

	profiles, err := mk.Profiles(ctx)
	if err != nil {
		return fmt.Errorf("failed to get profiles: %w", err)
	}

	profileName := cfg.Minikube.Profile
	if profileName == "" {
		profileName = "minikube"
	}

	profile, ok := profiles[profileName]
	if ok && profile.Status != "Stopped" {
		if profile.Status == "OK" {
			m.logger.Info("Cluster already running")

			return nil
		}

		return fmt.Errorf("%w: state=%s", ErrAlreadyExists, profile.Status)
	}

	mk.SetProfile(profileName)

	if err := mk.Start(ctx); err != nil {
		return fmt.Errorf("failed to start minikube: %w", err)
	}

	return nil
}

func (m *Manager) getConfig(name string) config.Cluster {
	for _, cluster := range m.cfg.Clusters {
		if cluster.Name == name {
			return cluster
		}
	}

	return nil
}

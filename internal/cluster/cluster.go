package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/briandowns/spinner"
	"github.com/csnewman/localflux/internal/config"
	"github.com/google/go-containerregistry/pkg/authn"
)

var (
	ErrNoDefault     = errors.New("no default cluster set")
	ErrNotDefined    = errors.New("cluster not defined in config")
	ErrAlreadyExists = errors.New("cluster already exists")
	ErrInvalidState  = errors.New("cluster in invalid state")
	ErrInvalidConfig = errors.New("invalid configuration")
)

type Status string

const (
	StatusNotFound Status = "not-found"
	StatusStopped  Status = "stopped"
	StatusActive   Status = "active"
)

type Provider interface {
	Status(ctx context.Context) (Status, error)

	Create(ctx context.Context) error

	Start(ctx context.Context) error

	Reconfigure(ctx context.Context) error

	ContextName() string

	BuildKitConfig() config.BuildKit

	Registry() string

	RegistryConn(ctx context.Context) (http.RoundTripper, authn.Authenticator, error)
}

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

func (m *Manager) Start(ctx context.Context, name string) error {
	if name == "" {
		name = m.cfg.DefaultCluster
	}

	if name == "" {
		return ErrNoDefault
	}

	p, err := m.Provider(name)
	if err != nil {
		return err
	}

	status, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	switch status {
	case StatusNotFound:
		m.logger.Info("Creating cluster", "name", name)

		if err := p.Create(ctx); err != nil {
			return fmt.Errorf("failed to create: %w", err)
		}

	case StatusActive:
		m.logger.Info("Cluster already running", "name", name)

		if err := p.Reconfigure(ctx); err != nil {
			return fmt.Errorf("failed to reconfigure: %w", err)
		}

	case StatusStopped:
		m.logger.Info("Starting cluster", "name", name)

		if err := p.Start(ctx); err != nil {
			return fmt.Errorf("failed to start: %w", err)
		}

	default:
		panic("unexpected status")
	}

	kc, err := NewK8sClientForCtx(DefaultKubeConfigPath(), p.ContextName())
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	m.logger.Info("Fetching flux manifests")

	fluxSrc, err := FetchFluxManifests(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch flux manifests: %w", err)
	}

	m.logger.Info("Applying flux manifests")

	if err := kc.Apply(ctx, fluxSrc); err != nil {
		return fmt.Errorf("failed to apply flux manifests: %w", err)
	}

	m.logger.Info("Waiting until cluster is ready")

	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
	s.Start()

	for _, ns := range []string{"kube-system", "flux-system"} {
		if err := kc.WaitNamespaceReady(ctx, ns, func(names []string) {
			str := ""

			for i, v := range names {
				if i != 0 {
					str += ", "
				}

				str += ns + "/" + v
			}

			s.Suffix = str
		}); err != nil {
			s.Stop()

			return fmt.Errorf("failed to wait for NS: %w", err)
		}
	}

	s.Stop()

	m.logger.Info("Ready")

	return nil
}

func (m *Manager) GetConfig(name string) (config.Cluster, error) {
	for _, cluster := range m.cfg.Clusters {
		if cluster.Name == name {
			return cluster, nil
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrNotDefined, name)
}

func (m *Manager) Provider(name string) (Provider, error) {
	cfg, err := m.GetConfig(name)
	if err != nil {
		return nil, err
	}

	if cfg.Minikube != nil {
		mc := NewMinikube(m.logger)
		mp := NewMinikubeProvider(m.logger, mc, cfg)

		return mp, nil
	}

	return nil, fmt.Errorf("%w: %s has no provider", ErrInvalidConfig, name)
}

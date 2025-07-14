package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/csnewman/localflux/internal/config"
	"github.com/csnewman/localflux/internal/crds"
	"github.com/google/go-containerregistry/pkg/authn"
	cmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const baseManifests = `
apiVersion: v1
kind: Namespace
metadata:
  labels:
    app.kubernetes.io/instance: localflux
    app.kubernetes.io/part-of: localflux
  name: localflux
`

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

type ProviderCallbacks struct {
	Step func(detail string)

	Success func(detail string)

	Info func(msg string)

	Warn func(msg string)

	Error func(msg string)
}

func (c ProviderCallbacks) NotifyStep(s string) {
	if c.Step != nil {
		c.Step(s)
	}
}

func (c ProviderCallbacks) NotifySuccess(s string) {
	if c.Success != nil {
		c.Success(s)
	}
}

func (c ProviderCallbacks) NotifyWarning(s string) {
	if c.Warn != nil {
		c.Warn(s)
	}
}

func (c ProviderCallbacks) NotifyError(s string) {
	if c.Error != nil {
		c.Error(s)
	}
}

type Provider interface {
	Status(ctx context.Context, cb ProviderCallbacks) (Status, error)

	Create(ctx context.Context, cb ProviderCallbacks) error

	Start(ctx context.Context, cb ProviderCallbacks) error

	Reconfigure(ctx context.Context, cb ProviderCallbacks) error

	ContextName() string

	KubeConfig() string

	BuildKitConfig() config.BuildKit

	BuildKitDialer(ctx context.Context, addr string) (net.Conn, error)

	RelayConfig() config.Relay

	RelayK8Config(ctx context.Context) (*cmdapi.Config, error)

	Registry() string

	RegistryConn(ctx context.Context) (http.RoundTripper, authn.Authenticator, error)

	Name() string
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

type Callbacks interface {
	Completed(msg string, dur time.Duration)

	State(msg string, detail string, start time.Time)

	Success(detail string)

	Info(msg string)

	Warn(msg string)

	Error(msg string)

	StepLines(lines []string)
}

func (m *Manager) Start(ctx context.Context, name string, cb Callbacks) error {
	start := time.Now()

	cb.State("Checking", "", start)

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

	status, err := p.Status(ctx, ProviderCallbacks{
		Step:    func(detail string) {},
		Success: cb.Success,
		Info:    cb.Info,
		Warn:    cb.Warn,
		Error:   cb.Error,
	})
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	cb.Info(fmt.Sprintf("Starting cluster %q using %q", name, p.Name()))

	start = time.Now()

	switch status {
	case StatusNotFound:
		m.logger.Info("Creating cluster", "name", name)

		cb.State("Creating cluster", "", start)

		if err := p.Create(ctx, ProviderCallbacks{
			Step: func(detail string) {
				cb.State("Creating cluster", detail, start)
			},
			Success: cb.Success,
			Info:    cb.Info,
			Warn:    cb.Warn,
			Error:   cb.Error,
		}); err != nil {
			return fmt.Errorf("failed to create: %w", err)
		}

	case StatusActive:
		m.logger.Info("Cluster already running", "name", name)

		cb.State("Reconfiguring existing cluster", "", start)

		if err := p.Reconfigure(ctx, ProviderCallbacks{
			Step: func(detail string) {
				cb.State("Reconfiguring existing cluster", detail, start)
			},
			Success: cb.Success,
			Info:    cb.Info,
			Warn:    cb.Warn,
			Error:   cb.Error,
		}); err != nil {
			return fmt.Errorf("failed to reconfigure: %w", err)
		}

	case StatusStopped:
		m.logger.Info("Starting cluster", "name", name)

		cb.State("Starting existing cluster", "", start)

		if err := p.Start(ctx, ProviderCallbacks{
			Step: func(detail string) {
				cb.State("Starting existing cluster", detail, start)
			},
			Success: cb.Success,
			Info:    cb.Info,
			Warn:    cb.Warn,
			Error:   cb.Error,
		}); err != nil {
			return fmt.Errorf("failed to start: %w", err)
		}

	default:
		panic("unexpected status")
	}

	cb.Completed("Cluster configured", time.Since(start))

	kc, err := NewK8sClientForCtx(p.KubeConfig(), p.ContextName())
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	start = time.Now()

	m.logger.Info("Fetching flux manifests")

	cb.State("Configuring flux", "Fetching manifests", start)

	fluxSrc, err := FetchFluxManifests(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch flux manifests: %w", err)
	}

	m.logger.Info("Applying flux manifests")

	cb.State("Configuring flux", "Applying", start)

	if err := kc.Apply(ctx, fluxSrc); err != nil {
		return fmt.Errorf("failed to apply flux manifests: %w", err)
	}

	cb.Completed("Flux configured", time.Since(start))

	start = time.Now()

	m.logger.Info("Applying localflux manifests")

	cb.State("Configuring localflux", "Applying CRDs", start)

	if err := kc.Apply(ctx, crds.All); err != nil {
		return fmt.Errorf("failed to apply crds: %w", err)
	}

	cb.State("Configuring localflux", "Applying manifests", start)

	if err := kc.Apply(ctx, baseManifests); err != nil {
		return fmt.Errorf("failed to apply base manifests: %w", err)
	}

	cb.Completed("Manifests configured", time.Since(start))

	relayConfig := p.RelayConfig()
	if relayConfig.Enabled {
		start = time.Now()

		m.logger.Info("Deploying relay")

		cb.State("Deploying relay", "Applying manifests", start)

		var rendered bytes.Buffer

		if err := relayManifests.Execute(&rendered, map[string]any{
			"hostNetwork": !relayConfig.ClusterNetworking,
		}); err != nil {
			return fmt.Errorf("failed to render relay manifests: %w", err)
		}

		if err := kc.Apply(ctx, rendered.String()); err != nil {
			return fmt.Errorf("failed to apply relay manifests: %w", err)
		}

		if !relayConfig.DisableClient {
			cb.State("Deploying relay", "Creating local container", start)

			rcfg, err := p.RelayK8Config(ctx)
			if err != nil {
				return fmt.Errorf("failed to get relay k8 config: %w", err)
			}

			if err := startRelay(ctx, m.logger, rcfg, cb); err != nil {
				return fmt.Errorf("failed to start relay: %w", err)
			}
		}

		cb.Completed("Relay configured", time.Since(start))
	}

	start = time.Now()

	m.logger.Info("Waiting until cluster is ready")

	if err := kc.WaitNamespaceReady(ctx, []string{"kube-system", "flux-system"}, func(names []string) {
		cb.State("Waiting until cluster is ready", strings.Join(names, ", "), start)
	}); err != nil {
		return fmt.Errorf("failed to wait for cluster: %w", err)
	}

	m.logger.Info("Cluster ready")

	cb.State("Cluster ready", "", start)
	cb.Completed("Cluster ready", time.Since(start))

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

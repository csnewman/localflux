package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/docker/cli/cli/connhelper/commandconn"
	"io"
	"k8s.io/client-go/tools/clientcmd"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/csnewman/localflux/internal/config"
	"github.com/csnewman/localflux/internal/config/v1alpha1"
	"github.com/google/go-containerregistry/pkg/authn"
	"golang.org/x/sync/errgroup"
	cmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var (
	ErrAddonNotFound = errors.New("addon not found")
	ErrAddonFailed   = errors.New("addon failed")
	ErrUnexpected    = errors.New("unexpected response")
)

type MinikubeProvider struct {
	logger *slog.Logger
	c      *Minikube
	cfg    config.Cluster
}

var _ Provider = (*MinikubeProvider)(nil)

func NewMinikubeProvider(logger *slog.Logger, c *Minikube, cfg config.Cluster) *MinikubeProvider {
	return &MinikubeProvider{
		logger: logger,
		c:      c,
		cfg:    cfg,
	}
}

func (p *MinikubeProvider) Name() string {
	return "minikube"
}

func (p *MinikubeProvider) ProfileName() string {
	name := p.cfg.Minikube.Profile
	if name != "" {
		return name
	}

	return "minikube"
}

func (p *MinikubeProvider) Status(ctx context.Context, cb ProviderCallbacks) (Status, error) {
	profiles, err := p.c.Profiles(ctx, cb)
	if err != nil {
		return "", err
	}

	profile, ok := profiles[p.ProfileName()]
	if !ok {
		return StatusNotFound, nil
	}

	switch profile.Status {
	case "OK", "Running":
		return StatusActive, nil
	case "Stopped":
		return StatusStopped, nil
	default:
		return "", fmt.Errorf("unknown profile status: %s", profile.Status)
	}
}

func (p *MinikubeProvider) Create(ctx context.Context, cb ProviderCallbacks) error {
	status, err := p.Status(ctx, cb)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status != StatusNotFound {
		return ErrAlreadyExists
	}

	if err := p.c.Start(ctx, p.ProfileName(), p.cfg.Minikube.CustomArgs, p.cfg.Minikube.CNI, cb); err != nil {
		return fmt.Errorf("failed to start minikube: %w", err)
	}

	return p.configureCommon(ctx, cb)
}

func (p *MinikubeProvider) Start(ctx context.Context, cb ProviderCallbacks) error {
	status, err := p.Status(ctx, cb)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status != StatusStopped {
		return fmt.Errorf("%w: %v", ErrInvalidState, status)
	}

	if err := p.c.Start(ctx, p.ProfileName(), p.cfg.Minikube.CustomArgs, p.cfg.Minikube.CNI, cb); err != nil {
		return fmt.Errorf("failed to start minikube: %w", err)
	}

	return p.configureCommon(ctx, cb)
}

func (p *MinikubeProvider) Reconfigure(ctx context.Context, cb ProviderCallbacks) error {
	status, err := p.Status(ctx, cb)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status != StatusActive {
		return fmt.Errorf("%w: %v", ErrInvalidState, status)
	}

	return p.configureCommon(ctx, cb)
}

const registryAliases = "registry-aliases"

var requiredMinikubeAddons = []string{
	"metrics-server",
	"storage-provisioner",
	"registry",
	registryAliases,
}

func (p *MinikubeProvider) configureCommon(ctx context.Context, cb ProviderCallbacks) error {
	cb.NotifyStep("Checking addons")

	profile := p.ProfileName()

	addons, err := p.c.Addons(ctx, profile)
	if err != nil {
		return fmt.Errorf("failed to list addons: %w", err)
	}

	var toEnable []string

	toEnable = append(toEnable, requiredMinikubeAddons...)

	for _, addon := range p.cfg.Minikube.Addons {
		if slices.Contains(toEnable, addon) {
			continue
		}

		toEnable = append(toEnable, addon)
	}

	for _, name := range toEnable {
		state, ok := addons[name]
		if !ok {
			return fmt.Errorf("%w: %s", ErrAddonNotFound, name)
		}

		if state {
			p.logger.Info("Addon is already enabled", "name", name)

			continue
		}

		p.logger.Info("Enabling addon", "name", name)

		cb.NotifyStep("Enabling addon: " + name)

		if name == registryAliases && len(p.cfg.Minikube.RegistryAliases) > 0 {
			if err := p.c.ConfigureRegistryAliases(ctx, profile, name, p.cfg.Minikube.RegistryAliases); err != nil {
				return fmt.Errorf("failed to configure addon %q: %w", name, err)
			}
		}

		if err := p.c.EnableAddon(ctx, profile, name); err != nil {
			return fmt.Errorf("failed to enable addon %q: %w", name, err)
		}

		cb.NotifySuccess("Enabled addon: " + name)
	}

	return nil
}

func (p *MinikubeProvider) ContextName() string {
	return p.ProfileName()
}

func (p *MinikubeProvider) KubeConfig() string {
	if p.cfg.SSH != nil {
		panic("todo")
	}

	return p.cfg.KubeConfig
}

func (p *MinikubeProvider) BuildKitConfig() config.BuildKit {
	if p.cfg.BuildKit == nil {
		return &v1alpha1.BuildKit{}
	}

	return p.cfg.BuildKit
}

func (p *MinikubeProvider) BuildKitDialer(ctx context.Context, addr string) (net.Conn, error) {
	var cmd []string

	if p.cfg.SSH != nil {
		cmd = append(cmd, "ssh", p.cfg.SSH.Address, "--")
	}

	cmd = append(cmd,
		"minikube", "--alsologtostderr", "ssh", "--native-ssh=false", "--profile", p.ProfileName(), "--",
		"sudo", "buildctl", "dial-stdio",
	)

	return commandconn.New(context.Background(), cmd[0], cmd[1:]...)
}

func (p *MinikubeProvider) RelayConfig() config.Relay {
	if p.cfg.Relay == nil {
		return &v1alpha1.Relay{}
	}

	if p.cfg.SSH != nil {
		panic("todo")
	}

	return p.cfg.Relay
}

func (p *MinikubeProvider) K8sClient(ctx context.Context) (*K8sClient, error) {
	if p.cfg.SSH == nil {
		// TODO: use same minikube config approach
		kc, err := NewK8sClientForCtx(p.KubeConfig(), p.ContextName())
		if err != nil {
			return nil, fmt.Errorf("failed to create k8s client: %w", err)
		}

		return kc, nil
	}

	ctxName := p.ContextName()

	raw, err := p.c.Config(ctx, p.ProfileName(), ctxName)
	if err != nil {
		panic(err)
	}

	p.logger.Debug("Raw k8s cfg", "raw", raw)

	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("config from bytes failed: %w", err)
	}

	loader := clientcmd.NewNonInteractiveClientConfig(
		*cfg,
		ctxName,
		&clientcmd.ConfigOverrides{
			CurrentContext: ctxName,
		},
		nil,
	)

	config, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	config.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		args := []string{
			p.cfg.SSH.Address,
			"--",
			"socat",
			"-",
			network + ":" + address,
		}

		return commandconn.New(context.Background(), "ssh", args...)

	}

	rawConfig, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	client, err := NewK8sClientFromConfig(config, rawConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	return client, nil
}

func (p *MinikubeProvider) RelayK8Config(ctx context.Context) (*cmdapi.Config, error) {
	if p.cfg.SSH != nil {
		panic("todo")
	}

	ip, err := p.c.IP(ctx, p.ProfileName())
	if err != nil {
		return nil, fmt.Errorf("failed to get ip: %w", err)
	}

	cfg, err := GetFlattenedConfig(
		p.KubeConfig(),
		p.ProfileName(),
	)
	if err != nil {
		return nil, err
	}

	if len(cfg.Clusters) != 1 {
		return nil, fmt.Errorf("expected 1 cluster, found %d", len(cfg.Clusters))
	}

	for _, cluster := range cfg.Clusters {
		u, err := url.Parse(cluster.Server)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cluster server URL: %w", err)
		}

		u.Host = ip.String()

		break
	}

	return cfg, nil
}

func (p *MinikubeProvider) Registry() string {
	return "registry.minikube"
}

func (p *MinikubeProvider) CNI() string {
	return p.cfg.Minikube.CNI
}

func (p *MinikubeProvider) RegistryConn(ctx context.Context) (http.RoundTripper, authn.Authenticator, error) {
	if p.cfg.SSH != nil {
		panic("todo")
	}

	ip, err := p.c.IP(ctx, p.ProfileName())
	if err != nil {
		return nil, nil, err
	}

	addrOverride := net.JoinHostPort(ip.String(), "5000")

	dc := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	trans := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, net, addr string) (net.Conn, error) {
			return dc(ctx, net, addrOverride)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   50,
	}

	return trans, authn.Anonymous, nil
}

type Minikube struct {
	logger *slog.Logger
	ssh    config.SSH
}

func NewMinikube(logger *slog.Logger, ssh config.SSH) *Minikube {
	return &Minikube{
		logger: logger,
		ssh:    ssh,
	}
}

func (m *Minikube) cmd(ctx context.Context) *exec.Cmd {
	if m.ssh == nil {
		return exec.CommandContext(ctx, "minikube")
	}

	return exec.CommandContext(ctx, "ssh", m.ssh.Address, "--", "minikube")

}

func (m *Minikube) Start(
	ctx context.Context,
	profile string,
	extraArgs []string,
	cni string,
	cb ProviderCallbacks,
) error {
	errgrp, ctx := errgroup.WithContext(ctx)

	c := m.cmd(ctx)

	c.Args = append(c.Args, "start")

	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	c.Args = append(c.Args, "--output", "json")
	c.Args = append(c.Args, "--driver", "docker")
	c.Args = append(c.Args, "--cpus", "no-limit")
	c.Args = append(c.Args, "--memory", "no-limit")

	if cni != "" {
		c.Args = append(c.Args, "--cni", cni)
	}

	c.Args = append(c.Args, extraArgs...)

	pr, pw := io.Pipe()
	prE, pwE := io.Pipe()
	c.Stdout = pw
	c.Stderr = pwE
	c.Stdin = nil

	errgrp.Go(func() error {
		return m.processOutput(pr, func(line string) (bool, error) {
			return false, nil
		}, cb)
	})

	errgrp.Go(func() error {
		return m.processErrOutput(prE, cb)
	})

	errgrp.Go(func() error {
		defer pw.Close()
		defer pwE.Close()

		return c.Run()
	})

	return errgrp.Wait()
}

type MinikubeProfile struct {
	Name   string
	Status string
}

type rawProfiles struct {
	Valid *[]rawProfile `json:"valid"`
}

type rawProfile struct {
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

func (m *Minikube) Profiles(ctx context.Context, cb ProviderCallbacks) (map[string]MinikubeProfile, error) {
	errgrp, ctx := errgroup.WithContext(ctx)

	c := m.cmd(ctx)

	c.Args = append(c.Args, "profile")
	c.Args = append(c.Args, "list")
	c.Args = append(c.Args, "--output", "json")

	pr, pw := io.Pipe()
	prE, pwE := io.Pipe()
	c.Stdout = pw
	c.Stderr = pwE
	c.Stdin = nil

	profiles := make(map[string]MinikubeProfile)

	errgrp.Go(func() error {
		return m.processOutput(pr, func(line string) (bool, error) {
			var raw rawProfiles

			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				// Ignore
				return false, nil
			}

			if raw.Valid == nil {
				return false, nil
			}

			for _, profile := range *raw.Valid {
				profiles[profile.Name] = MinikubeProfile{
					Name:   profile.Name,
					Status: profile.Status,
				}
			}

			return true, nil
		}, ProviderCallbacks{})
	})

	errgrp.Go(func() error {
		return m.processErrOutput(prE, cb)
	})

	errgrp.Go(func() error {
		defer pw.Close()
		defer pwE.Close()

		return c.Run()
	})

	if err := errgrp.Wait(); err != nil {
		return nil, err
	}

	return profiles, nil
}

type rawAddon struct {
	Profile string `json:"Profile"`
	Status  string `json:"Status"`
}

func (m *Minikube) Addons(ctx context.Context, profile string) (map[string]bool, error) {
	errgrp, ctx := errgroup.WithContext(ctx)

	c := m.cmd(ctx)

	c.Args = append(c.Args, "addons")
	c.Args = append(c.Args, "list")

	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	c.Args = append(c.Args, "--output", "json")

	pr, pw := io.Pipe()
	prE, pwE := io.Pipe()
	c.Stdout = pw
	c.Stderr = pwE
	c.Stdin = nil

	addons := make(map[string]bool)

	errgrp.Go(func() error {
		return m.processOutput(pr, func(line string) (bool, error) {
			var raw map[string]rawAddon

			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				// Ignore
				return false, nil
			}

			found := false

			for name, entry := range raw {
				if entry.Profile != profile {
					continue
				}

				found = true
				addons[name] = entry.Status == "enabled"
			}

			return found, nil
		}, ProviderCallbacks{})
	})

	errgrp.Go(func() error {
		return m.processErrOutput(prE, ProviderCallbacks{})
	})

	errgrp.Go(func() error {
		defer pw.Close()
		defer pwE.Close()

		return c.Run()
	})

	if err := errgrp.Wait(); err != nil {
		return nil, err
	}

	return addons, nil
}

func (m *Minikube) EnableAddon(ctx context.Context, profile string, name string) error {
	c := m.cmd(ctx)

	c.Args = append(c.Args, "addons")
	c.Args = append(c.Args, "enable")

	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	c.Args = append(c.Args, name)

	buffer := bytes.NewBuffer(nil)
	bufferErr := bytes.NewBuffer(nil)

	c.Stdout = buffer
	c.Stderr = bufferErr
	c.Stdin = nil

	if err := c.Run(); err != nil {
		return err
	}

	text := buffer.String()

	if strings.Contains(text, "addon is enabled") {
		return nil
	}

	m.logger.Info("Unexpected output", "stdout", text, "stderr", bufferErr.String())

	return ErrAddonFailed
}

func (m *Minikube) Config(ctx context.Context, profile string, context string) (string, error) {
	c := m.cmd(ctx)
	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	c.Args = append(c.Args, "kubectl", "--", "config", "view", "--flatten", "--minify", "--context", context)

	buffer := bytes.NewBuffer(nil)
	bufferErr := bytes.NewBuffer(nil)

	c.Stdout = buffer
	c.Stderr = bufferErr
	c.Stdin = nil

	if err := c.Run(); err != nil {
		return "", err
	}

	text := buffer.String()

	if strings.Contains(text, "clusters:") {
		return text, nil
	}

	m.logger.Info("Unexpected output", "stdout", text, "stderr", bufferErr.String())

	return "", ErrUnexpected
}

func (m *Minikube) ConfigureRegistryAliases(ctx context.Context, profile string, name string, values []string) error {
	c := m.cmd(ctx)

	c.Args = append(c.Args, "addons")
	c.Args = append(c.Args, "configure")

	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	c.Args = append(c.Args, name)

	buffer := bytes.NewBuffer(nil)
	bufferErr := bytes.NewBuffer(nil)

	c.Stdout = buffer
	c.Stderr = bufferErr

	c.Stdin = strings.NewReader(strings.Join(values, " ") + "\n")

	if err := c.Run(); err != nil {
		return err
	}

	text := buffer.String()

	if strings.Contains(text, "successfully configured") {
		return nil
	}

	m.logger.Info("Unexpected output", "stdout", text, "stderr", bufferErr.String())

	return ErrAddonFailed
}

func (m *Minikube) IP(ctx context.Context, profile string) (net.IP, error) {
	c := m.cmd(ctx)
	c.Args = append(c.Args, "ip")

	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	buffer := bytes.NewBuffer(nil)
	bufferErr := bytes.NewBuffer(nil)

	c.Stdout = buffer
	c.Stderr = bufferErr
	c.Stdin = nil

	if err := c.Run(); err != nil {
		m.logger.Info("Unexpected output", "stdout", buffer.String(), "stderr", bufferErr.String())

		return nil, err
	}

	text := strings.TrimSpace(buffer.String())

	ip := net.ParseIP(text)

	if ip == nil {
		m.logger.Info("Unexpected output", "stdout", buffer.String(), "stderr", bufferErr.String())

		return nil, ErrInvalidState
	}

	return ip, nil
}

func (m *Minikube) processOutput(pr *io.PipeReader, processor func(line string) (bool, error), cb ProviderCallbacks) error {
	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		text := scanner.Text()

		ok, err := processor(text)
		if err != nil {
			return err
		}

		if ok {
			continue
		}

		event := cloudevents.NewEvent()

		if err := json.Unmarshal([]byte(text), &event); err != nil {
			m.logger.Error("Failed to unmarshal event", "raw", text)
			continue
		}

		if event.DataContentType() != "application/json" {
			continue
		}

		switch event.Type() {
		case "io.k8s.sigs.minikube.step":
			var data map[string]string
			err := event.DataAs(&data)
			if err != nil {
				m.logger.Error("Failed to unmarshal event", "event", event.Type(), "raw", text)
				continue
			}

			m.logger.Info("Minikube step", "step", data["name"])

			cb.NotifyStep(data["name"])

		case "io.k8s.sigs.minikube.info":
			var data map[string]string
			err := event.DataAs(&data)
			if err != nil {
				m.logger.Error("Failed to unmarshal event", "event", event.Type(), "raw", text)
				continue
			}

			m.logger.Info("Minikube info", "msg", data["message"])
		case "io.k8s.sigs.minikube.warning":
			var data map[string]string
			err := event.DataAs(&data)
			if err != nil {
				m.logger.Error("Failed to unmarshal event", "event", event.Type(), "raw", text)
				continue
			}

			m.logger.Info("Minikube warning", "msg", data["message"])

			cb.NotifyWarning(data["message"])

		case "io.k8s.sigs.minikube.error":
			var data map[string]string
			err := event.DataAs(&data)
			if err != nil {
				m.logger.Error("Failed to unmarshal event", "event", event.Type(), "raw", text)
				continue
			}

			m.logger.Info("Minikube error", "msg", data["message"])

			cb.NotifyError(data["message"])

		default:
			m.logger.Error("Unknown event type", "event", event.Type())
		}
	}

	return nil
}

func (m *Minikube) processErrOutput(pr *io.PipeReader, cb ProviderCallbacks) error {
	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		text := scanner.Text()

		if len(strings.TrimSpace(text)) == 0 {
			continue
		}

		m.logger.Warn("Minikube std err output", "output", text)

		cb.NotifyWarning("Minikube stderr: " + text)
	}

	return nil
}

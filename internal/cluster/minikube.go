package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/csnewman/localflux/internal/config"
	"io"
	"log/slog"
	"os"
	"os/exec"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"golang.org/x/sync/errgroup"
)

type MinikubeProvider struct {
	c   *Minikube
	cfg config.Cluster
}

var _ Provider = (*MinikubeProvider)(nil)

func NewMinikubeProvider(c *Minikube, cfg config.Cluster) *MinikubeProvider {
	return &MinikubeProvider{
		c:   c,
		cfg: cfg,
	}
}

func (p *MinikubeProvider) ProfileName() string {
	name := p.cfg.Minikube.Profile
	if name != "" {
		return name
	}

	return "minikube"
}

func (p *MinikubeProvider) Status(ctx context.Context) (Status, error) {
	profiles, err := p.c.Profiles(ctx)
	if err != nil {
		return "", err
	}

	profile, ok := profiles[p.ProfileName()]
	if !ok {
		return StatusNotFound, nil
	}

	switch profile.Status {
	case "OK":
		return StatusActive, nil
	default:
		return "", fmt.Errorf("unknown profile status: %s", profile.Status)
	}
}

func (p *MinikubeProvider) Create(ctx context.Context) error {
	status, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status != StatusNotFound {
		return ErrAlreadyExists
	}

	return p.c.Start(ctx, p.ProfileName())
}

func (p *MinikubeProvider) Start(ctx context.Context) error {
	status, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status != StatusStopped {
		return fmt.Errorf("%w: %v", ErrInvalidState, status)
	}

	return p.c.Start(ctx, p.ProfileName())
}

func (p *MinikubeProvider) ContextName() string {
	return p.ProfileName()
}

type Minikube struct {
	logger *slog.Logger
}

func NewMinikube(logger *slog.Logger) *Minikube {
	return &Minikube{
		logger: logger,
	}
}

func (m *Minikube) Start(ctx context.Context, profile string) error {
	errgrp, ctx := errgroup.WithContext(ctx)

	c := exec.CommandContext(ctx, "minikube")

	c.Args = append(c.Args, "start")

	if profile != "" {
		c.Args = append(c.Args, "--profile", profile)
	}

	c.Args = append(c.Args, "--output", "json")
	c.Args = append(c.Args, "--driver", "docker")
	c.Args = append(c.Args, "--cpus", "no-limit")
	c.Args = append(c.Args, "--memory", "no-limit")

	pr, pw := io.Pipe()
	c.Stdout = pw
	c.Stderr = os.Stderr
	c.Stdin = nil

	errgrp.Go(func() error {
		return m.processOutput(pr, func(line string) (bool, error) {
			return false, nil
		})
	})

	errgrp.Go(func() error {
		defer pw.Close()

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

func (m *Minikube) Profiles(ctx context.Context) (map[string]MinikubeProfile, error) {
	errgrp, ctx := errgroup.WithContext(ctx)

	c := exec.CommandContext(ctx, "minikube")

	c.Args = append(c.Args, "profile")
	c.Args = append(c.Args, "list")
	c.Args = append(c.Args, "--output", "json")

	pr, pw := io.Pipe()
	c.Stdout = pw
	c.Stderr = os.Stderr
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
		})
	})

	errgrp.Go(func() error {
		defer pw.Close()

		return c.Run()
	})

	if err := errgrp.Wait(); err != nil {
		return nil, err
	}

	return profiles, nil
}

func (m *Minikube) processOutput(pr *io.PipeReader, processor func(line string) (bool, error)) error {
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

		case "io.k8s.sigs.minikube.error":
			var data map[string]string
			err := event.DataAs(&data)
			if err != nil {
				m.logger.Error("Failed to unmarshal event", "event", event.Type(), "raw", text)
				continue
			}

			m.logger.Info("Minikube error", "msg", data["message"])

		default:
			m.logger.Error("Unknown event type", "event", event.Type())
		}
	}

	return nil
}

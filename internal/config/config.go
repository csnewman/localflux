package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/csnewman/localflux/internal/config/v1alpha1"
	"sigs.k8s.io/yaml"
)

type (
	Config     = *v1alpha1.Config
	Cluster    = *v1alpha1.Cluster
	BuildKit   = *v1alpha1.BuildKit
	Image      = *v1alpha1.Image
	Deployment = *v1alpha1.Deployment
	Step       = *v1alpha1.Step
)

var ErrUnknownVersion = errors.New("unknown version")

type Wrapper struct {
	Version string `json:"apiVersion"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var w Wrapper

	if err := yaml.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}

	if w.Version != v1alpha1.Version {
		return nil, fmt.Errorf("%w: %s", ErrUnknownVersion, w.Version)
	}

	var cfg v1alpha1.Config

	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}

	return &cfg, nil
}

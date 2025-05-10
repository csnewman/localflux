package deployment

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/csnewman/localflux/internal/config"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/credentials"
	"github.com/docker/cli/cli/connhelper/commandconn"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/connhelper"
	"github.com/moby/buildkit/cmd/buildctl/build"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"
)

func init() {
	connhelper.Register("cmd", func(url *url.URL) (*connhelper.ConnectionHelper, error) {
		return &connhelper.ConnectionHelper{
			ContextDialer: func(ctx context.Context, addr string) (net.Conn, error) {
				addr = strings.TrimPrefix(addr, "cmd://")
				parts := strings.Split(addr, "/")

				return commandconn.New(context.Background(), parts[0], parts[1:]...)
			},
		}, nil
	})
}

type Builder struct {
	logger     *slog.Logger
	cfg        config.BuildKit
	c          *client.Client
	attachable []session.Attachable
}

func NewBuilder(ctx context.Context, logger *slog.Logger, cfg config.BuildKit) (*Builder, error) {
	address := cfg.Address

	if address == "" {
		if _, err := exec.LookPath("docker"); err == nil {
			address = "cmd://docker/buildx/dial-stdio"
		}
	}

	c, err := client.New(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to buildkit: %w", err)
	}

	dockerConfig, err := dockerconfig.Load(cfg.DockerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load docker config: %w", err)
	}

	if !dockerConfig.ContainsAuth() {
		dockerConfig.CredentialsStore = credentials.DetectDefaultStore(dockerConfig.CredentialsStore)
	}

	tlsConfigs, err := build.ParseRegistryAuthTLSContext(cfg.RegistryAuthTLSContext)
	if err != nil {
		return nil, fmt.Errorf("failed to parse registry tls auth context: %w", err)
	}

	attachable := []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		ConfigFile:       dockerConfig,
		TLSConfigs:       tlsConfigs,
		ExpireCachedAuth: nil,
	})}

	return &Builder{
		logger:     logger,
		cfg:        cfg,
		c:          c,
		attachable: attachable,
	}, nil
}

type Artifact struct {
	File *os.File
	Name string
}

func (a *Artifact) Delete() {
	_ = os.Remove(a.File.Name())
}

type SolveStatus = client.SolveStatus

func (b *Builder) Build(ctx context.Context, cfg config.Image, baseDir string, fn func(res *SolveStatus)) (*Artifact, error) {
	buildCtx := cfg.Context
	if buildCtx == "" {
		buildCtx = baseDir
	}

	buildFile := cfg.File
	if buildFile == "" {
		buildFile = filepath.Join(buildCtx, "Dockerfile")
	}

	cxtLocalMount, err := fsutil.NewFS(buildCtx)
	if err != nil {
		return nil, fmt.Errorf("invalid build context: %w", err)
	}

	dockerfileLocalMount, err := fsutil.NewFS(filepath.Dir(buildFile))
	if err != nil {
		return nil, fmt.Errorf("invalid dockerfile path: %w", err)
	}

	frontendAttrs := map[string]string{
		"source":   "docker/dockerfile",
		"filename": filepath.Base(buildFile),
	}

	if cfg.Target != "" {
		frontendAttrs["target"] = cfg.Target
	}

	for k, v := range cfg.BuildArgs {
		frontendAttrs["build-arg:"+k] = v
	}

	tempFile, err := os.CreateTemp("", "localflux-build")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	solveOpt := client.SolveOpt{
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterDocker,
				Attrs: map[string]string{
					"name": cfg.Image,
				},
				Output: func(vals map[string]string) (io.WriteCloser, error) {
					return tempFile, nil
				},
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"context":    cxtLocalMount,
			"dockerfile": dockerfileLocalMount,
		},
		Frontend:      "gateway.v0",
		FrontendAttrs: frontendAttrs,
		Session:       b.attachable,
	}

	statusChan := make(chan *client.SolveStatus)

	errgrp, gctx := errgroup.WithContext(ctx)

	var resp *client.SolveResponse

	errgrp.Go(func() error {
		var err error

		resp, err = b.c.Solve(gctx, nil, solveOpt, statusChan)

		return err
	})

	errgrp.Go(func() error {
		for {
			ss, ok := <-statusChan
			if !ok {
				return nil
			}

			fn(ss)
		}
	})

	err = errgrp.Wait()

	_ = tempFile.Close()

	if err != nil {
		_ = os.Remove(tempFile.Name())

		return nil, fmt.Errorf("failed to solve build: %w", err)
	}

	return &Artifact{
		File: tempFile,
		Name: resp.ExporterResponse["image.name"],
	}, nil
}

package deployment

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/config"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/credentials"
	"github.com/docker/cli/cli/connhelper/commandconn"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/connhelper"
	"github.com/moby/buildkit/cmd/buildctl/build"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/util/staticfs"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
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

func NewBuilder(ctx context.Context, logger *slog.Logger, provider cluster.Provider) (*Builder, error) {
	cfg := provider.BuildKitConfig()

	addr := cfg.Address

	const fallback = "localflux://fallback"

	if addr == "" {
		addr = fallback
	}

	c, err := client.New(ctx, addr, client.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		if addr == fallback {
			addr = ""
		}

		return provider.BuildKitDialer(ctx, addr)
	}))
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
	Name   string
	Digest string
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

	cxtLocalMount, err = fsutil.NewFilterFS(cxtLocalMount, &fsutil.FilterOpt{
		IncludePatterns: cfg.IncludePaths,
		ExcludePatterns: cfg.ExcludePaths,
	})
	if err != nil {
		return nil, fmt.Errorf("invalid filter: %w", err)
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

	solveOpt := client.SolveOpt{
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name":              cfg.Image,
					"registry.insecure": "true",
					"push":              "true",
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
	if err != nil {
		return nil, err
	}

	b.logger.Info("Build complete", "response", resp.ExporterResponse)

	return &Artifact{
		Name:   resp.ExporterResponse["image.name"],
		Digest: resp.ExporterResponse["containerimage.digest"],
	}, nil
}

func (b *Builder) BuildOCI(
	ctx context.Context,
	baseDir string,
	includePaths []string,
	excludePaths []string,
	image string,
	fn func(res *SolveStatus),
) (*Artifact, error) {
	cxtLocalMount, err := fsutil.NewFS(baseDir)
	if err != nil {
		return nil, fmt.Errorf("invalid build context: %w", err)
	}

	if len(includePaths) == 0 {
		includePaths = nil
	}

	if len(excludePaths) == 0 {
		excludePaths = nil
	}

	cxtLocalMount, err = fsutil.NewFilterFS(cxtLocalMount, &fsutil.FilterOpt{
		IncludePatterns: includePaths,
		ExcludePatterns: excludePaths,
	})
	if err != nil {
		return nil, fmt.Errorf("invalid filter: %w", err)
	}

	dockerfileLocalMount := staticfs.NewFS()
	dockerfileLocalMount.Add(
		"Dockerfile",
		&fstypes.Stat{
			Mode: 0600,
			Path: "Dockerfile",
		},
		[]byte(`FROM scratch
COPY * .`),
	)

	solveOpt := client.SolveOpt{
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name":              image,
					"registry.insecure": "true",
					"push":              "true",
					"oci-artifact":      "true",
				},
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"context":    cxtLocalMount,
			"dockerfile": dockerfileLocalMount,
		},
		Frontend: "gateway.v0",
		FrontendAttrs: map[string]string{
			"source":   "docker/dockerfile",
			"filename": "Dockerfile",
		},
		Session: b.attachable,
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
	if err != nil {
		return nil, err
	}

	b.logger.Info("Build complete", "response", resp.ExporterResponse)

	return &Artifact{
		Name:   resp.ExporterResponse["image.name"],
		Digest: resp.ExporterResponse["containerimage.digest"],
	}, nil
}

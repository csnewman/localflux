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
	"github.com/docker/cli/cli/connhelper/commandconn"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/connhelper"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"
)

func init() {
	connhelper.Register("cmd", func(url *url.URL) (*connhelper.ConnectionHelper, error) {
		return &connhelper.ConnectionHelper{
			ContextDialer: func(ctx context.Context, addr string) (net.Conn, error) {
				addr = strings.TrimPrefix(addr, "cmd://")
				parts := strings.Split(addr, "/")

				return commandconn.New(nil, parts[0], parts[1:]...)
			},
		}, nil
	})
}

type Builder struct {
	logger *slog.Logger
	cfg    config.BuildKit
	c      *client.Client
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

	return &Builder{
		logger: logger,
		cfg:    cfg,
		c:      c,
	}, nil
}

type Artifact struct {
	File *os.File
	Name string
}

func (a *Artifact) Delete() {
	_ = os.Remove(a.File.Name())
}

func (b *Builder) Build(ctx context.Context, cfg config.Image, baseDir string, statusChan chan *client.SolveStatus) (*Artifact, error) {
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
		close(statusChan)

		return nil, fmt.Errorf("invalid build context: %w", err)
	}

	dockerfileLocalMount, err := fsutil.NewFS(filepath.Dir(buildFile))
	if err != nil {
		close(statusChan)

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
		close(statusChan)

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
	}

	resp, err := b.c.Solve(ctx, nil, solveOpt, statusChan)
	if err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())

		return nil, fmt.Errorf("failed to solve build: %w", err)
	}

	_ = tempFile.Close()

	return &Artifact{
		File: tempFile,
		Name: resp.ExporterResponse["image.name"],
	}, nil
}

func DisplayProgress(ctx context.Context, statusChan chan *client.SolveStatus) error {
	d, err := progressui.NewDisplay(os.Stderr, progressui.TtyMode)
	if err != nil {
		d, _ = progressui.NewDisplay(os.Stdout, progressui.PlainMode)
	}

	_, err = d.UpdateFrom(ctx, statusChan)

	return err
}

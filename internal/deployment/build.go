package deployment

import (
	"context"
	"fmt"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/csnewman/localflux/internal/config"
	"github.com/docker/cli/cli/connhelper/commandconn"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/connhelper"
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

func (b *Builder) Build(ctx context.Context, cfg config.Image, baseDir string, fn func(bg *BuildGraph)) (*Artifact, error) {
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
	}

	statusChan := make(chan *client.SolveStatus)

	errgrp, gctx := errgroup.WithContext(ctx)

	var resp *client.SolveResponse

	errgrp.Go(func() error {
		var err error

		resp, err = b.c.Solve(gctx, nil, solveOpt, statusChan)

		return err
	})

	bg := &BuildGraph{
		Nodes: map[string]*Node{},
	}

	errgrp.Go(func() error {
		for {
			select {
			case msg, ok := <-statusChan:
				if !ok {
					bg.Mu.Lock()
					bg.Final = true
					bg.Mu.Unlock()

					fn(bg)

					return nil
				}

				processStatus(bg, msg)

				fn(bg)

			case <-ctx.Done():
				return ctx.Err()
			}
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

type BuildGraph struct {
	Mu sync.Mutex

	Nodes     map[string]*Node
	NodeOrder []string
	Final     bool
}

func (g *BuildGraph) Node(d digest.Digest) *Node {
	id := d.String()

	n, ok := g.Nodes[id]
	if ok {
		return n
	}

	n = &Node{
		ID:       id,
		Statuses: make(map[string]*NodeStatus),
	}

	g.Nodes[id] = n

	return n
}

type Node struct {
	ID          string
	Name        string
	Started     *time.Time
	Completed   *time.Time
	Cached      bool
	Error       string
	Statuses    map[string]*NodeStatus
	StatusOrder []string
	Logs        []byte
	Warnings    []*NodeWarning
}

func (n *Node) Status(id string) *NodeStatus {
	s, ok := n.Statuses[id]
	if ok {
		return s
	}

	s = &NodeStatus{}

	n.Statuses[id] = s

	return s
}

type NodeStatus struct {
	Started   *time.Time
	Completed *time.Time
	Current   int64
	Total     int64
}

type NodeWarning struct {
	Level int
	Short []byte
}

func processStatus(bg *BuildGraph, msg *client.SolveStatus) {
	bg.Mu.Lock()
	defer bg.Mu.Unlock()

	for _, vertex := range msg.Vertexes {
		n := bg.Node(vertex.Digest)

		n.Name = vertex.Name
		n.Error = vertex.Error

		if vertex.Cached {
			n.Cached = true
		}

		if n.Started == nil && vertex.Started != nil {
			n.Started = vertex.Started
			bg.NodeOrder = append(bg.NodeOrder, n.ID)
		}

		if vertex.Completed != nil && (n.Completed == nil || (*n.Completed).Before(*vertex.Completed)) {
			n.Completed = vertex.Completed
		}
	}

	for _, status := range msg.Statuses {
		n := bg.Node(status.Vertex)
		s := n.Status(status.ID)

		if s.Started == nil && status.Started != nil {
			s.Started = status.Started

			n.StatusOrder = append(n.StatusOrder, status.ID)
		}

		if s.Completed != nil && (s.Completed == nil || (*s.Completed).Before(*status.Completed)) {
			s.Completed = status.Completed
		}

		s.Current = status.Current
		s.Total = status.Total
	}

	for _, log := range msg.Logs {
		n := bg.Node(log.Vertex)

		n.Logs = append(n.Logs, log.Data...)
	}

	for _, warn := range msg.Warnings {
		n := bg.Node(warn.Vertex)

		n.Warnings = append(n.Warnings, &NodeWarning{
			Level: warn.Level,
			Short: warn.Short,
		})
	}
}

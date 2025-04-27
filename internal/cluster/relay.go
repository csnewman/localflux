package cluster

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/tools/clientcmd"
	cmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const relayManifests = `
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app.kubernetes.io/component: relay
    app.kubernetes.io/instance: localflux
    app.kubernetes.io/part-of: localflux
  name: relay
  namespace: localflux
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: relay
      app.kubernetes.io/instance: localflux
      app.kubernetes.io/part-of: localflux
  template:
    metadata:
      labels:
        app.kubernetes.io/component: relay
        app.kubernetes.io/instance: localflux
        app.kubernetes.io/part-of: localflux
    spec:
      containers:
      - name: localflux
        image: ghcr.io/csnewman/localflux:master
        imagePullPolicy: Always
        args:
        - "relay-server"
        - "--debug"
      priorityClassName: system-cluster-critical
`

func startRelay(ctx context.Context, logger *slog.Logger, rcfg *cmdapi.Config, cb Callbacks) error {
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", "localflux-relay").Run()

	eg, ctx := errgroup.WithContext(ctx)

	data, err := clientcmd.Write(*rcfg)
	if err != nil {
		return fmt.Errorf("failed to marshal relay config: %w", err)
	}

	b64 := base64.StdEncoding.EncodeToString(data)

	cmd := exec.CommandContext(
		ctx,
		"docker",
		"run",
		"-d",
		"--network", "host",
		"--name", "localflux-relay",
		"--pull", "always",
		"ghcr.io/csnewman/localflux:master",
		"relay",
		"--debug",
		rcfg.CurrentContext,
		"--kube-cfg-b64",
		b64,
	)

	or, ow := io.Pipe()
	er, ew := io.Pipe()

	cmd.Stdout = ow
	cmd.Stderr = ew

	eg.Go(func() error {
		defer ow.Close()
		defer ew.Close()

		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("cmd error: %w", err)
		}

		return nil
	})

	var stdLines []string

	eg.Go(func() error {
		s := bufio.NewScanner(or)

		for s.Scan() {
			txt := s.Text()

			if strings.TrimSpace(txt) == "" {
				continue
			}

			logger.Debug("Docker stdout", "line", txt)

			stdLines = append(stdLines, txt)
		}

		return nil
	})

	eg.Go(func() error {
		s := bufio.NewScanner(er)

		var lines []string

		for s.Scan() {
			lines = append(lines, s.Text())

			cb.StepLines(lines)

			logger.Debug("Docker stderr", "line", s.Text())
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return err
	}

	cb.StepLines(nil)

	if len(stdLines) == 1 {
		cb.Info(fmt.Sprintf("Created relay container: %q", stdLines[0]))
	}

	return nil
}

package cluster

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/tools/clientcmd"
	cmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const relayManifests = `
apiVersion: v1
kind: Namespace
metadata:
  labels:
    app.kubernetes.io/instance: localflux
    app.kubernetes.io/part-of: localflux
  name: localflux
---
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
        imagePullPolicy: IfNotPresent
        args:
        - "relay-server"
        - "--debug"
      priorityClassName: system-cluster-critical
`

func startRelay(ctx context.Context, logger *slog.Logger, rcfg *cmdapi.Config, cb Callbacks) error {
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", "localflux-relay").Run()

	eg, ctx := errgroup.WithContext(ctx)

	//data, err := json.Marshal(rcfg)
	//if err != nil {
	//	return fmt.Errorf("failed to marshal relay config: %w", err)
	//}

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

	eg.Go(func() error {
		s := bufio.NewScanner(or)

		for s.Scan() {
			cb.Info(fmt.Sprintf("info: %s", s.Text()))
		}

		return nil
	})

	eg.Go(func() error {
		s := bufio.NewScanner(er)

		for s.Scan() {
			cb.Info(fmt.Sprintf("err: %s", s.Text()))
			//logger.Info("relay", "msg", s.Text())
		}

		return nil
	})

	return eg.Wait()
}

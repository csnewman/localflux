package main

import (
	"context"
	"fmt"
	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/config"
	"github.com/csnewman/localflux/internal/deployment"
	"github.com/spf13/cobra"
)

func createDeployCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy configuration",
		RunE:  deploy,
		Args:  cobra.MaximumNArgs(1),
	}

	c.Flags().String("cluster", "", "Cluster name")

	return c
}

func deploy(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load("localflux.yaml")
	if err != nil {
		return err
	}

	cm := cluster.NewManager(logger, cfg)

	m := deployment.NewManager(logger, cfg, cm)

	cluster, err := cmd.Flags().GetString("cluster")
	if err != nil {
		return fmt.Errorf("failed to parse cluster flag: %w", err)
	}

	var name string

	if len(args) > 0 {
		name = args[0]
	}

	return drive(cmd.Context(), func(ctx context.Context, cb driverCallbacks) error {
		return m.Deploy(ctx, cluster, name, cb)
	})
}

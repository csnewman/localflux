package main

import (
	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/config"
	"github.com/spf13/cobra"
)

func createClusterCmd() *cobra.Command {
	start := &cobra.Command{
		Use:   "start [name]",
		Short: "Start a cluster",
		RunE:  clusterStart,
		Args:  cobra.MaximumNArgs(1),
	}

	c := &cobra.Command{
		Use:   "cluster",
		Short: "Manage clusters",
	}

	c.AddCommand(start)

	return c
}

func clusterStart(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load("localflux.yaml")
	if err != nil {
		return err
	}

	m := cluster.NewManager(logger, cfg)

	var name string

	if len(args) > 0 {
		name = args[0]
	}

	return m.Start(cmd.Context(), name)
}

package main

import (
	"context"
	"fmt"
	"github.com/csnewman/localflux/internal/relay"
	"github.com/spf13/cobra"
)

func createRelayCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "relay [context]",
		Short: "relay services",
		RunE:  relayRun,
		Args:  cobra.ExactArgs(1),
	}

	c.Flags().String("kube-cfg-b64", "", "Base64 encoded kube config")

	return c
}

func relayRun(cmd *cobra.Command, args []string) error {
	name := args[0]

	c := relay.NewClient(logger)

	cfgB64, err := cmd.Flags().GetString("kube-cfg-b64")
	if err != nil {
		return fmt.Errorf("failed to parse kube-cfg-b64 flag: %w", err)
	}

	return drive(cmd.Context(), func(ctx context.Context, cb driverCallbacks) error {
		return c.Run(ctx, name, cfgB64, cb)
	})
}

func createRelayServerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "relay-server",
		Short:  "Server component for relay",
		RunE:   relayServerRun,
		Args:   cobra.ExactArgs(0),
		Hidden: true,
	}

	return c
}

func relayServerRun(cmd *cobra.Command, _ []string) error {
	s := relay.NewServer(logger)

	return s.Run(cmd.Context())
}

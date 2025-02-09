package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var logger *slog.Logger

func main() {
	rootCmd := &cobra.Command{
		Use:   "localflux",
		Short: "Simple and fast local k8s development",
		Long: `
Simple and fast local k8s development.
See https://github.com/csnewman/localflux
`,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			}))

			return nil
		},
	}

	rootCmd.AddCommand(createClusterCmd())
	rootCmd.AddCommand(createDeployCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

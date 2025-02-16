package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var logger *slog.Logger

var (
	plainOutput bool
	debugOutput bool
)

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
			if debugOutput {
				plainOutput = true

				logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
					Level: slog.LevelDebug,
				}))
			} else {
				logger = slog.New(slog.DiscardHandler)
			}

			return nil
		},
	}

	rootCmd.PersistentFlags().BoolVar(&debugOutput, "debug", false, "output debug info")
	rootCmd.PersistentFlags().BoolVar(&plainOutput, "plain", false, "disable fancy output")

	rootCmd.AddCommand(createClusterCmd())
	rootCmd.AddCommand(createDeployCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

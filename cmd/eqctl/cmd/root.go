// Package cmd implements the eqctl operator CLI for EntroQ.
package cmd

import (
	"log"
	"os"

	"github.com/shiblon/entroq/pkg/version"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "eqctl",
	Version: version.Version,
	Short:   "EntroQ operator CLI for deployment and configuration tasks.",
	Long: `eqctl is the operator CLI for EntroQ. It provides tools for
deploying and configuring EntroQ services, including OPA policy setup
and schema management.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

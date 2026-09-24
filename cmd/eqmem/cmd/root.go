// Package cmd holds the commands for the eqmem application.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/shiblon/entroq/cmd/internal/eqflags"
	"github.com/shiblon/entroq/pkg/version"
	"github.com/spf13/cobra"
)

var (
	cfgFile  string
	settings = eqflags.NewEnvironment("EQMEM")
)

var rootCmd = &cobra.Command{
	Use:               "eqmem",
	Version:           version.Version,
	Short:             "In-memory EntroQ service. Run 'eqmem serve' to start.",
	PersistentPreRunE: eqflags.Apply(settings),
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqmem.yml)")
}

func initConfig() {
	if cfgFile == "" {
		cfgFile = settings.GetString("config")
	}
	if cfgFile != "" {
		settings.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		settings.AddConfigPath(filepath.Join(home, ".config"))
		settings.SetConfigName("eqmem.yml")
	}
	if err := settings.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", settings.ConfigFileUsed())
	}
}

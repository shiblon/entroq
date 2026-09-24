// Package cmd holds the commands for the eqsqlite application.
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
	settings = eqflags.NewEnvironment("EQSQLITE")
	dbPath   string
)

var rootCmd = &cobra.Command{
	Use:               "eqsqlite",
	Version:           version.Version,
	Short:             "Experimental SQLite-backed EntroQ service.",
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
	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqsqlite.yml)")
	pflags.StringVar(&dbPath, "path", "entroq.db", "SQLite database path. Overrides EQ_SQLITE_PATH.")
}

func resolveSQLiteFlags() {
	if !rootCmd.PersistentFlags().Changed("path") {
		if path := os.Getenv("EQ_SQLITE_PATH"); path != "" {
			dbPath = path
		}
	}
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
		settings.SetConfigName("eqsqlite.yml")
	}
	if err := settings.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", settings.ConfigFileUsed())
	}
}

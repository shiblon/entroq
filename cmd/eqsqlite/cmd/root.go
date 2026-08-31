// Package cmd holds the commands for the eqsqlite application.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/shiblon/entroq/pkg/version"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string
	dbPath  string
)

var rootCmd = &cobra.Command{
	Use:     "eqsqlite",
	Version: version.Version,
	Short:   "Experimental SQLite-backed EntroQ service.",
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
	viper.BindPFlag("path", pflags.Lookup("path"))
}

func resolveSQLiteFlags() {
	if !rootCmd.PersistentFlags().Changed("path") {
		if path := os.Getenv("EQ_SQLITE_PATH"); path != "" {
			dbPath = path
		} else if viper.IsSet("path") {
			dbPath = viper.GetString("path")
		}
	}
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqsqlite.yml")
	}
	viper.AutomaticEnv()
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

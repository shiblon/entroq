// Package cmd holds the commands for the eqredis application.
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
	cfgFile  string
	redisAddr string
	redisPwd  string
	redisDB   int
)

var rootCmd = &cobra.Command{
	Use:     "eqredis",
	Version: version.Version,
	Short:   "Redis-backed EntroQ: run 'eqredis serve' to start the service.",
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
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqredis.yml)")
	pflags.StringVar(&redisAddr, "addr", "localhost:6379", "Redis server address. Overrides EQ_REDIS_ADDR environment.")
	pflags.StringVar(&redisPwd, "password", "", "Redis AUTH password. Overrides EQ_REDIS_PASSWORD environment.")
	pflags.IntVar(&redisDB, "db", 0, "Redis database number.")

	viper.BindPFlag("addr", pflags.Lookup("addr"))
	viper.BindPFlag("password", pflags.Lookup("password"))
	viper.BindPFlag("db", pflags.Lookup("db"))
}

// resolveRedisFlags fills connection fields from environment when flags were
// not set explicitly. Call this at the top of any RunE that needs Redis.
// Environment variables: EQ_REDIS_ADDR, EQ_REDIS_PASSWORD.
func resolveRedisFlags() {
	if redisAddr == "localhost:6379" {
		if v := os.Getenv("EQ_REDIS_ADDR"); v != "" {
			redisAddr = v
		}
	}
	if redisPwd == "" {
		redisPwd = os.Getenv("EQ_REDIS_PASSWORD")
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
		viper.SetConfigName("eqredis.yml")
	}
	viper.AutomaticEnv()
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string

	dbAddr string
	dbName string
	dbUser string
	dbPass string
)

var rootCmd = &cobra.Command{
	Use:   "eqpg",
	Short: "PostgreSQL-backed EntroQ: service management and schema utilities.",
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
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqpg)")
	pflags.StringVar(&dbAddr, "dbaddr", ":5432", "Address of PostgreSQL server. Overrides PGHOST:PGPORT environments.")
	pflags.StringVar(&dbName, "dbname", "", "Name of database that houses tasks. Overrides PGDATABASE environment.")
	pflags.StringVar(&dbUser, "dbuser", "", "Database user name. Overrides PGUSER environment.")
	pflags.StringVar(&dbPass, "dbpwd", "", "Database password. Overrides PGPASSWORD environment.")

	viper.BindPFlag("dbaddr", pflags.Lookup("dbaddr"))
	viper.BindPFlag("dbname", pflags.Lookup("dbname"))
	viper.BindPFlag("dbuser", pflags.Lookup("dbuser"))
	viper.BindPFlag("dbpwd", pflags.Lookup("dbpwd"))
}

// resolveDBFlags fills in DB connection variables from environment when the
// corresponding flags were not set explicitly. Call this at the top of any
// RunE that needs a database connection.
func resolveDBFlags() {
	if dbPass == "" {
		dbPass = os.Getenv("PGPASSWORD")
	}
	if dbName == "" {
		dbName = os.Getenv("PGDATABASE")
	}
	if dbUser == "" {
		dbUser = os.Getenv("PGUSER")
	}
	if dbAddr == "" {
		dbAddr = os.Getenv("PGHOST") + ":" + os.Getenv("PGPORT")
	}
}

// initConfig reads in config file and ENV variables if set.
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
		viper.SetConfigName("eqpg.yml")
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

package cmd

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/shiblon/entroq/cmd/internal/eqflags"
	"github.com/shiblon/entroq/pkg/backend/eqpg"
	"github.com/shiblon/entroq/pkg/version"
	"github.com/spf13/cobra"
)

var (
	cfgFile  string
	settings = eqflags.NewEnvironment("EQPG")

	dbAddr string
	dbName string
	dbUser string
	dbPass string
	dbURL  string

	dbSSLMode     string
	dbSSLRootCert string
	dbSSLCert     string
	dbSSLKey      string
)

var rootCmd = &cobra.Command{
	Use:               "eqpg",
	Version:           version.Version,
	Short:             "PostgreSQL-backed EntroQ: service management and schema utilities.",
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
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqpg)")
	pflags.StringVar(&dbAddr, "dbaddr", "", "Address of PostgreSQL server. Overrides PGHOST:PGPORT environments; defaults to :5432.")
	pflags.StringVar(&dbName, "dbname", "", "Name of database that houses tasks. Overrides PGDATABASE environment.")
	pflags.StringVar(&dbUser, "dbuser", "", "Database user name. Overrides PGUSER environment.")
	pflags.StringVar(&dbPass, "dbpwd", "", "Database password. Overrides PGPASSWORD environment.")
	pflags.StringVar(&dbURL, "dburl", "", "PostgreSQL connection URL. Overrides PGURL and decomposed database settings; prefer PGURL when it contains credentials.")
	pflags.StringVar(&dbSSLMode, "dbsslmode", "", "PostgreSQL TLS mode. Overrides PGSSLMODE environment.")
	pflags.StringVar(&dbSSLRootCert, "dbsslrootcert", "", "PostgreSQL TLS root certificate file. Overrides PGSSLROOTCERT environment.")
	pflags.StringVar(&dbSSLCert, "dbsslcert", "", "PostgreSQL TLS client certificate file. Overrides PGSSLCERT environment.")
	pflags.StringVar(&dbSSLKey, "dbsslkey", "", "PostgreSQL TLS client key file. Overrides PGSSLKEY environment.")
}

// resolveDBFlags fills in DB connection variables from environment when the
// corresponding flags were not set explicitly. Call this at the top of any
// RunE that needs a database connection.
func resolveDBFlags() {
	if dbURL == "" {
		dbURL = os.Getenv("PGURL")
	}
	if dbURL != "" {
		return
	}
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
		port := os.Getenv("PGPORT")
		if port == "" {
			port = "5432"
		}
		dbAddr = net.JoinHostPort(os.Getenv("PGHOST"), port)
	}
	if dbSSLMode == "" {
		dbSSLMode = os.Getenv("PGSSLMODE")
	}
	if dbSSLRootCert == "" {
		dbSSLRootCert = os.Getenv("PGSSLROOTCERT")
	}
	if dbSSLCert == "" {
		dbSSLCert = os.Getenv("PGSSLCERT")
	}
	if dbSSLKey == "" {
		dbSSLKey = os.Getenv("PGSSLKEY")
	}
}

// databaseConnection resolves the authoritative database target and the
// connection options used with it. A URL is complete, so decomposed options
// are only supplied for a host:port target.
func databaseConnection() (string, []eqpg.PGOpt) {
	resolveDBFlags()
	if dbURL != "" {
		return dbURL, nil
	}
	options := []eqpg.PGOpt{
		eqpg.WithDB(dbName),
		eqpg.WithUsername(dbUser),
		eqpg.WithPassword(dbPass),
	}
	if dbSSLMode != "" {
		options = append(options, eqpg.WithSSL(eqpg.SSLMode(dbSSLMode)))
	}
	if dbSSLRootCert != "" {
		options = append(options, eqpg.WithSSLServerCAFile(dbSSLRootCert))
	}
	if dbSSLCert != "" || dbSSLKey != "" {
		options = append(options, eqpg.WithSSLClientFiles(dbSSLCert, dbSSLKey))
	}
	return dbAddr, options
}

func databaseDescription(target string) string {
	if !strings.Contains(target, "://") {
		return fmt.Sprintf("postgres(%s db=%s user=%s)", dbAddr, dbName, dbUser)
	}
	u, err := url.Parse(target)
	if err != nil {
		return "postgres(url)"
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	return fmt.Sprintf("postgres(%s db=%s user=%s)", u.Host, strings.TrimPrefix(u.Path, "/"), user)
}

// initConfig reads in config file and ENV variables if set.
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
		settings.SetConfigName("eqpg.yml")
	}

	if err := settings.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", settings.ConfigFileUsed())
	}
}

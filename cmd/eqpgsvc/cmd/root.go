// Copyright © 2019 NAME HERE <EMAIL ADDRESS>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"entrogo.com/entroq/backend/eqpg"
	"entrogo.com/entroq/pkg/authz/opahttp"
	"entrogo.com/entroq/qsvc"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	pb "entrogo.com/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"

	_ "github.com/lib/pq"
)

var (
	cfgFile  string
	port     int
	httpPort int

	maxSize int

	dbAddr   string
	dbName   string
	dbUser   string
	dbPass   string
	attempts int

	authzStrategy string
	opaURL        string
	opaPath       string
)

const (
	MB = 1024 * 1024
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqpgsvc",
	Short: "A postgres-backed EntroQ service.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		var authzOpt qsvc.Option
		switch authzStrategy {
		case "opahttp":
			authzOpt = qsvc.WithAuthorizer(opahttp.New(
				opahttp.WithHostURL(opaURL),
				opahttp.WithAPIPath(opaPath),
			))
		case "", "none":
			authzOpt = qsvc.WithAuthorizer(nil)
		default:
			return fmt.Errorf("Unknown Authz strategy: %q", authzStrategy)
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
			dbAddr = os.Getenv("PGHOST") + ":" + os.Getenv("PGPORT")
		}

		opener := eqpg.Opener(dbAddr,
			eqpg.WithDB(dbName),
			eqpg.WithUsername(dbUser),
			eqpg.WithPassword(dbPass),
			eqpg.WithConnectAttempts(attempts),
		)

		svc, err := qsvc.New(ctx, opener, authzOpt)
		if err != nil {
			return fmt.Errorf("failed to open pg backend: %w", err)
		}
		defer svc.Close()

		go func() {
			http.Handle("/metrics", promhttp.Handler())
			log.Fatalf("http and metric server: %v", http.ListenAndServe(fmt.Sprintf(":%d", httpPort), nil))
		}()

		lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", port))
		if err != nil {
			return fmt.Errorf("error listening on port %d: %w", port, err)
		}

		s := grpc.NewServer(
			grpc.MaxRecvMsgSize(maxSize*MB),
			grpc.MaxSendMsgSize(maxSize*MB),
		)
		pb.RegisterEntroQServer(s, svc)
		hpb.RegisterHealthServer(s, health.NewServer())
		log.Printf("Starting EntroQ server %d -> %v db=%v u=%v", port, dbAddr, dbName, dbUser)
		return s.Serve(lis)
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqpgsvc)")
	pflags.IntVar(&port, "port", 37706, "Service port number.")
	pflags.IntVar(&httpPort, "http_port", 9100, "Port to listen to HTTP requests on, including for /metrics.")
	pflags.StringVar(&dbAddr, "dbaddr", ":5432", "Address of PostgreSQL server. Overrides PGHOST:PGPORT environments.")
	pflags.StringVar(&dbName, "dbname", "", "Name of database that houses tasks. Overrides PGNAME environment.")
	pflags.StringVar(&dbUser, "dbuser", "", "Database user name. Overrides PGUSER environment.")
	pflags.StringVar(&dbPass, "dbpwd", "", "Database password. Overrides PGPASSWORD environment.")
	pflags.IntVar(&attempts, "attempts", 10, "Connection attempts, separated by 5-second pauses, before dying due to lack of backend connection.")
	pflags.IntVar(&maxSize, "max_size_mb", 10, "Maximum server message size (send and receive) in megabytes. If larger than 4MB, you must also set your gRPC client max size to take advantage of this.")
	pflags.StringVar(&authzStrategy, "authz", "none", "Strategy to use for authorization. Default is no authorization, everything allowed by every rquester.")
	pflags.StringVar(&opaURL, "opa_url", "", fmt.Sprintf("Base (scheme://host:port) URL for talking to OPA. Leave blank for default value %s.", opahttp.DefaultHostURL))
	pflags.StringVar(&opaPath, "opa_path", "", fmt.Sprintf("Path for OPA API access. Leave blank for default path %s.", opahttp.DefaultAPIPath))

	viper.BindPFlag("port", pflags.Lookup("port"))
	viper.BindPFlag("http_port", pflags.Lookup("http_port"))
	viper.BindPFlag("dbaddr", pflags.Lookup("dbaddr"))
	viper.BindPFlag("dbname", pflags.Lookup("dbname"))
	viper.BindPFlag("dbuser", pflags.Lookup("dbuser"))
	viper.BindPFlag("dbpwd", pflags.Lookup("dbpwd"))
	viper.BindPFlag("attempts", pflags.Lookup("attempts"))
	viper.BindPFlag("max_size_mb", pflags.Lookup("max_size_mb"))
	viper.BindPFlag("authz", pflags.Lookup("authz"))
	viper.BindPFlag("opa_base_url", pflags.Lookup("opa_base_url"))
	viper.BindPFlag("opa_path", pflags.Lookup("opa_path"))
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqpgsvc.yml")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

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

	"entrogo.com/entroq/pg"
	"entrogo.com/entroq/qsvc"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
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

	dbAddr   string
	dbName   string
	dbUser   string
	dbPass   string
	attempts int
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqpgsvc",
	Short: "A postgres-backed EntroQ service.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		svc, err := qsvc.New(ctx, pg.Opener(dbAddr,
			pg.WithDB(dbName),
			pg.WithUsername(dbUser),
			pg.WithPassword(dbPass),
			pg.WithConnectAttempts(attempts),
		))
		if err != nil {
			return errors.Wrap(err, "failed to open pg backend")
		}
		defer svc.Close()

		go func() {
			http.Handle("/metrics", promhttp.Handler())
			log.Fatalf("http and metric server: %v", http.ListenAndServe(fmt.Sprintf(":%d", httpPort), nil))
		}()

		lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", port))
		if err != nil {
			return errors.Wrapf(err, "error listening on port %d", port)
		}

		s := grpc.NewServer()
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
	pflags.IntVar(&httpPort, "http_port", 8080, "Port to listen to HTTP requests on, including for /metrics.")
	pflags.StringVar(&dbAddr, "dbaddr", ":5432", "Address of PostgreSQL server.")
	pflags.StringVar(&dbName, "dbname", "postgres", "Database housing tasks.")
	pflags.StringVar(&dbUser, "dbuser", "postgres", "Database user name.")
	pflags.StringVar(&dbPass, "dbpwd", "postgres", "Database password.")
	pflags.IntVar(&attempts, "attempts", 10, "Connection attempts, separated by 5-second pauses, before dying due to lack of backend connection.")

	viper.BindPFlag("port", pflags.Lookup("port"))
	viper.BindPFlag("http_port", pflags.Lookup("http_port"))
	viper.BindPFlag("dbaddr", pflags.Lookup("dbaddr"))
	viper.BindPFlag("dbname", pflags.Lookup("dbname"))
	viper.BindPFlag("dbuser", pflags.Lookup("dbuser"))
	viper.BindPFlag("dbpwd", pflags.Lookup("dbpwd"))
	viper.BindPFlag("attempts", pflags.Lookup("attempts"))
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

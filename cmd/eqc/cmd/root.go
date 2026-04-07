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
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqgrpc"
	"github.com/shiblon/entroq/backend/eqpg"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var rootFlags struct {
	cfgFile      string
	svcAddr      string
	secure       bool
	authzToken   string
	maxMsgSizeMB int
	pgURL        string
	pgHeartbeat  string
}

var eq *entroq.EntroQ

// cliJSON converts a CLI string value to a json.RawMessage. The string must
// be valid JSON; an empty string becomes null.
func cliJSON(s string) (json.RawMessage, error) {
	if s == "" {
		return nil, nil
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("value is not valid JSON: %q", s)
	}
	return json.RawMessage(s), nil
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqc [options] <command>",
	Short: "EntroQ Client CLI for poking at an EntroQ gRPC service",
	Long: `The eqc CLI allows you to query an EntroQ service, getting
queue listings, individual task information, etc.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Name() == "help" || cmd.IsAdditionalHelpTopicCommand() {
			return nil
		}
		var opts []eqgrpc.Option
		if rootFlags.secure {
			pool, err := x509.SystemCertPool()
			if err != nil {
				return fmt.Errorf("cert pool: %w", err)
			}
			creds := credentials.NewClientTLSFromCert(pool, "")
			opts = append(opts, eqgrpc.WithDialOpts(grpc.WithTransportCredentials(creds)))
		} else {
			opts = append(opts, eqgrpc.WithInsecure())
		}

		if rootFlags.authzToken != "" {
			opts = append(opts, eqgrpc.WithBearerToken(rootFlags.authzToken))
		}

		if size := rootFlags.maxMsgSizeMB; size != 0 {
			opts = append(opts, eqgrpc.WithMaxSize(size))
		}

		var err error
		if rootFlags.pgURL != "" {
			var hb time.Duration
			if rootFlags.pgHeartbeat != "" {
				if hb, err = time.ParseDuration(rootFlags.pgHeartbeat); err != nil {
					return fmt.Errorf("pg heartbeat duration Parse: %w", err)
				}
			}
			eq, err = entroq.New(context.Background(), eqpg.Opener(rootFlags.pgURL, eqpg.WithHeartbeat(hb)))
		} else {
			eq, err = entroq.New(context.Background(), eqgrpc.Opener(rootFlags.svcAddr, opts...))
		}
		if err != nil {
			return fmt.Errorf("entroq client open: %w", err)
		}
		return nil
	},
	PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
		if eq != nil {
			return eq.Close()
		}
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.
	rootCmd.PersistentFlags().StringVar(&rootFlags.cfgFile, "config", "", "config file (default is $HOME/.config/eqc.yaml)")

	rootCmd.PersistentFlags().IntVar(&rootFlags.maxMsgSizeMB, "max_msg_size_mb", 10, "Maximum gRPC message size in megabytes.")

	rootCmd.PersistentFlags().StringVarP(&rootFlags.svcAddr, "svcaddr", "s", ":37706", "address of service, uses port 37706 if none is specified")
	rootCmd.PersistentFlags().BoolVarP(&rootFlags.secure, "secure", "S", false, "Use secure connection.")
	rootCmd.PersistentFlags().StringVar(&rootFlags.authzToken, "authz_token", "", "Pass an Authorization token.")
	rootCmd.PersistentFlags().StringVar(&rootFlags.pgURL, "pg_url", "", "PostgreSQL URL for direct backend connection.")
	rootCmd.PersistentFlags().StringVar(&rootFlags.pgHeartbeat, "pg_heartbeat", "", "Heartbeat interval for direct PG connection (e.g. 5s).")

	viper.BindPFlag("svcaddr", rootCmd.PersistentFlags().Lookup("svcaddr"))
	viper.BindPFlag("authz_token", rootCmd.PersistentFlags().Lookup("authz_token"))
	viper.BindPFlag("max_msg_size_mb", rootCmd.PersistentFlags().Lookup("max_msg_size_mb"))
	viper.BindPFlag("pg_url", rootCmd.PersistentFlags().Lookup("pg_url"))
	viper.BindPFlag("pg_heartbeat", rootCmd.PersistentFlags().Lookup("pg_heartbeat"))
}

func mustTaskString(t *entroq.Task) string {
	b, err := json.Marshal(t)
	if err != nil {
		log.Fatalf("Failed to marshal to JSON: %v", err)
	}
	return string(b)
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if rootFlags.cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(rootFlags.cfgFile)
	} else {
		// Find home directory.
		home, err := os.UserHomeDir()
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".eqc" (without extension).
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqc")
	}

	if !strings.Contains(rootFlags.svcAddr, ":") {
		rootFlags.svcAddr += ":37706"
	}

	viper.AutomaticEnv() // read in environment variables that match

	// Update core flags from viper (handles env vars).
	rootFlags.svcAddr = viper.GetString("svcaddr")
	rootFlags.authzToken = viper.GetString("authz_token")
	rootFlags.maxMsgSizeMB = viper.GetInt("max_msg_size_mb")
	rootFlags.pgURL = viper.GetString("pg_url")
	rootFlags.pgHeartbeat = viper.GetString("pg_heartbeat")

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		log.Print("Using config file:", viper.ConfigFileUsed())
	}
}

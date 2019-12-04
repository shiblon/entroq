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
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"

	"entrogo.com/entroq"
	"entrogo.com/entroq/grpc"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile   string
	svcAddr   string
	jsonValue bool

	eq *entroq.EntroQ
)

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
		var err error
		eq, err = entroq.New(context.Background(), grpc.Opener(svcAddr, grpc.WithInsecure()))
		if err != nil {
			return errors.Wrap(err, "entroq client open")
		}
		return nil
	},
	PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
		if eq != nil {
			return eq.Close()
		}
		return nil
	},
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
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqc.yaml)")

	rootCmd.PersistentFlags().StringVar(&svcAddr, "svcaddr", ":37706", "address of service, uses port 37706 if none is specified")
	rootCmd.PersistentFlags().BoolVarP(&jsonValue, "json", "j", false, "Display task values as JSON.")
}

func mustTaskString(t *entroq.Task) string {
	b, err := json.Marshal(t)
	if err != nil {
		log.Fatalf("Failed to marshal to JSON: %v", err)
	}

	if !jsonValue {
		return string(b)
	}

	// Unmarshal into a map so we can insert the value where we want it (as we want it).
	var tm map[string]interface{}
	var vm interface{}
	if err := json.Unmarshal(b, &tm); err != nil {
		log.Fatalf("Error creating map from JSON: %v", err)
	}
	v, err := base64.StdEncoding.DecodeString(tm["value"].(string))
	if err != nil {
		log.Fatalf("Failed to b64 deserialize byte value: %v", err)
	}
	if err := json.Unmarshal([]byte(v), &vm); err != nil {
		log.Fatalf("Failed to unmarshal JSON value: %v", err)
	}
	tm["value"] = vm

	b, err = json.Marshal(tm)
	if err != nil {
		log.Fatalf("Error marshaling unnested task: %v", err)
	}
	return string(b)
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
			log.Print(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".eqc" (without extension).
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqc")
	}

	if !strings.Contains(svcAddr, ":") {
		svcAddr += ":37706"
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		log.Print("Using config file:", viper.ConfigFileUsed())
	}
}

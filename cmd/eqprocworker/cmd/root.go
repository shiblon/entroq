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
	"os"
	"path/filepath"
	"strings"

	"entrogo.com/entroq"
	"entrogo.com/entroq/contrib/pkg/procworker"
	"entrogo.com/entroq/grpc"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string
	eqaddr  string
	inbox   string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqprocworker [options]",
	Short: "EntroQ Worker CLI that reads a subprocess task and runs the specified command",
	Long: `The eqprocworker processes EntroQ tasks and runs subprocess commands.

	It starts an EntroQ worker that accepts a subprocess definition and tries
	to run it. Use with care only in trusted environments, as this can open up
	privileged execution depending on the user running this worker and the
	users allowed to insert tasks.

	The task should have the following format, in valid JSON:
		{
			"outbox": "<name of done queue>",
			"errbox": "<name of error queue>",
			"cmd": ["path/to/cmd", "arg1", "arg2", "arg3"],
			"dir": "<working dir, defaults to parent of cmd>",
			"env": ["name=value", "name2=value2"]
		}

	If the outbox is not specified, it will be set to

	  <inbox>/done

	If the errbox is not specified, it is the same as the outbox.

	You can also specify "outdir" and "errdir". If these are specified, stdout
	and/or stderr will be written to files in those directories instead of
	inlined in the response task. The "outfile" and "errfile" parameters will
	point to those files.

	A task, if something is wrong with it, may also end up in one of the following:

	  <inbox>/failed-parse
	  <errbox>/failed-start

	A finished task will repeat the data from the input task (flags, etc.), and
	will additionally include the exit code (0 for success, as is typical):

		{
			"cmd": ["path/to/cmd", "arg1", "arg2", "arg3"]
			"error": "",
			"stdout": "standard output from subprocess",
			"stderr": "standard error from subprocess"
		}
	`,
	Run: func(cmd *cobra.Command, args []string) {
		if inbox == "" {
			log.Fatal("No inbox specified.")
		}
		ctx := context.Background()
		eq, err := entroq.New(ctx, grpc.Opener(eqaddr, grpc.WithInsecure()))
		if err != nil {
			log.Fatalf("Error opening EntroQ service: %v", err)
		}
		defer eq.Close()

		log.Printf("Starting worker for %q on inbox %q", eqaddr, inbox)
		if err := eq.NewWorker(inbox).Run(ctx, procworker.Run); err != nil {
			log.Fatalf("Error executing worker: %v", err)
		}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error executing root command: %v", err)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqprocworker.yaml)")

	pflags.StringVar(&inbox, "inbox", "/subprocworker/inbox", "Queue to listen to")
	pflags.StringVar(&eqaddr, "eqaddr", ":37706", "address of service, uses port 37706 if none is specified")

	viper.BindPFlag("eqaddr", pflags.Lookup("svcaddr"))
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

		// Search config in home directory with name ".config".
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqprocworker")
	}

	if !strings.Contains(eqaddr, ":") {
		eqaddr += ":37706"
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

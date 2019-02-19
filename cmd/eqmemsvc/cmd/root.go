// Package cmd holds the commands for the eqmemsvc application.
package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"

	"entrogo.com/entroq/mem"
	"entrogo.com/entroq/qsvc"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	pb "entrogo.com/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Flags.
var (
	cfgFile string
	port    int
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqmemsvc",
	Short: "A memory-backed EntroQ service. Ephemeral - don't trust to keep your data.",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			log.Fatalf("Error listening on port %d: %v", port, err)
		}

		svc, err := qsvc.New(ctx, mem.Opener())
		if err != nil {
			log.Fatalf("Failed to open backend for qsvc: %v", err)
		}
		defer svc.Close()

		s := grpc.NewServer()
		pb.RegisterEntroQServer(s, svc)
		hpb.RegisterHealthServer(s, health.NewServer())
		log.Fatal(s.Serve(lis))
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
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqmemsvc)")
	pflags.IntVar(&port, "port", 37706, "Port to listen on.")

	viper.BindPFlag("port", pflags.Lookup("port"))
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

		// Search config in home directory with name ".config/eqmemsvc" (without extension).
		viper.AddConfigPath(home)
		viper.SetConfigName(".config/eqmemsvc")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

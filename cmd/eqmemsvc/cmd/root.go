// Package cmd holds the commands for the eqmemsvc application.
package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"entrogo.com/entroq/mem"
	"entrogo.com/entroq/pkg/authz/opahttp"
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
)

// Flags.
var flags struct {
	cfgFile       string
	port          int
	httpPort      int
	authzStrategy string
	opaURL        string
	opaPath       string

	maxSize int
}

const (
	MB = 1024 * 1024
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqmemsvc",
	Short: "A memory-backed EntroQ service. Ephemeral - don't trust to keep your data.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", flags.port))
		if err != nil {
			return errors.Wrapf(err, "error listening on port %d", flags.port)
		}

		var authzOpt qsvc.Option
		switch flags.authzStrategy {
		case "opahttp":
			authzOpt = qsvc.WithAuthorizer(opahttp.New(
				opahttp.WithHostURL(flags.opaURL),
				opahttp.WithAPIPath(flags.opaPath),
			))
		case "", "none":
			authzOpt = qsvc.WithAuthorizer(nil)
		default:
			return fmt.Errorf("Unknown Authz strategy: %q", flags.authzStrategy)
		}

		svc, err := qsvc.New(ctx, mem.Opener(), authzOpt)
		if err != nil {
			return errors.Wrap(err, "failed to open mem backend for qsvc")
		}
		defer svc.Close()

		go func() {
			http.Handle("/metrics", promhttp.Handler())
			log.Fatalf("http and metric server: %v", http.ListenAndServe(fmt.Sprintf(":%d", flags.httpPort), nil))
		}()

		s := grpc.NewServer(
			grpc.MaxRecvMsgSize(flags.maxSize*MB),
			grpc.MaxSendMsgSize(flags.maxSize*MB),
		)
		pb.RegisterEntroQServer(s, svc)
		hpb.RegisterHealthServer(s, health.NewServer())
		log.Printf("Starting EntroQ server %d -> mem", flags.port)
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
	pflags.StringVar(&flags.cfgFile, "config", "", "config file (default is $HOME/.config/eqmemsvc)")
	pflags.IntVar(&flags.port, "port", 37706, "Port to listen on.")
	pflags.IntVar(&flags.httpPort, "http_port", 9100, "Port to listen to HTTP requests on, including for /metrics.")
	pflags.IntVar(&flags.maxSize, "max_size_mb", 10, "Maximum server message size (send and receive) in megabytes. If larger than 4MB, you must also set your gRPC client max size to take advantage of this.")
	pflags.StringVar(&flags.authzStrategy, "authz", "", "Strategy to use for authorization. Default is no authorization, everything allowed by every rquester.")
	pflags.StringVar(&flags.opaURL, "opa_url", "", fmt.Sprintf("Base (scheme://host:port) URL for talking to OPA. Leave blank for default value %s.", opahttp.DefaultHostURL))
	pflags.StringVar(&flags.opaPath, "opa_path", "", fmt.Sprintf("Path for OPA API access. Leave blank for default path %s.", opahttp.DefaultAPIPath))

	viper.BindPFlag("port", pflags.Lookup("port"))
	viper.BindPFlag("http_port", pflags.Lookup("http_port"))
	viper.BindPFlag("max_size_mb", pflags.Lookup("max_size_mb"))
	viper.BindPFlag("authz", pflags.Lookup("authz"))
	viper.BindPFlag("opa_base_url", pflags.Lookup("opa_base_url"))
	viper.BindPFlag("opa_path", pflags.Lookup("opa_path"))
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if flags.cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(flags.cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".config/eqmemsvc" (without extension).
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqmemsvc.yml")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

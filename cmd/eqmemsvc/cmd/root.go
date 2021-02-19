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
	"entrogo.com/entroq/qsvc"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	pb "entrogo.com/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Flags.
var (
	cfgFile  string
	port     int
	httpPort int

	maxSize int
)

const (
	MB = 1024 * 1024
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eqmemsvc",
	Short: "A memory-backed EntroQ service. Ephemeral - don't trust to keep your data.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return errors.Wrapf(err, "error listening on port %d", port)
		}

		svc, err := qsvc.New(ctx, mem.Opener())
		if err != nil {
			return errors.Wrap(err, "failed to open mem backend for qsvc")
		}
		defer svc.Close()

		go func() {
			http.Handle("/metrics", promhttp.Handler())
			log.Fatalf("http and metric server: %v", http.ListenAndServe(fmt.Sprintf(":%d", httpPort), nil))
		}()

		// TODO
		const authKey = "authorization"

		// TODO
		getAuthzToken := func(ctx context.Context) string {
			md, ok := metadata.FromIncomingContext(ctx)
			if !ok {
				return ""
			}
			vals := md[authKey]
			if len(vals) == 0 {
				return ""
			}
			return vals[0]
		}

		// TODO: returns JSON of the request, and the authorization header.
		unaryOPARequest := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo) ([]byte, error) {
			authzReq := &pb.AuthzRequest{Authz: &pb.Authz{Token: getAuthzToken(ctx)}}
			switch v := req.(type) {
			case *pb.ClaimRequest:
				authzReq.ClaimQueues = append(authzReq.ClaimQueues, v.Queues...)
			case *pb.ModifyRequest:
				// TODO: empty queues are always meaningful, we must insert
				// them into the list. That way the authorizer can tell that
				// someone is trying to do a "queueless admin" operation.
				for _, ins := range v.Inserts {
					authzReq.InsertQueues = append(authzReq.InsertQueues, ins.Queue)
				}
				for _, chg := range v.Changes {
					authzReq.ChangeQueues = append(authzReq.ChangeQueues, chg.GetNewData().Queue, chg.GetOldId().Queue)
				}
				for _, del := range v.Deletes {
					authzReq.DeleteQueues = append(authzReq.DeleteQueues, del.Queue)
				}
				for _, dep := range v.Deletes {
					authzReq.DeleteQueues = append(authzReq.DeleteQueues, dep.Queue)
				}
				// TODO: send queue names for delete/depend stuff, too?
				// We could make the queue name optional there, and then
				// explicitly add the "empty queue" when it isn't set. Then
				// there could be a blanket "let it through, they're admin"
				// sort of setting for when the queue is left out. Other than
				// that, there' really no reason they shouldn't know what queue
				// they're deleting from, changing, or depending on.
			case *pb.TasksRequest:
				authzReq.TasksQueues = append(authzReq.TasksQueues, v.Queue)
			}

			b, err := protojson.Marshal(authzReq)
			if err != nil {
				return nil, errors.Wrap(err, "marshal authz req")
			}
			return b, nil
		}

		s := grpc.NewServer(
			grpc.MaxRecvMsgSize(maxSize*MB),
			grpc.MaxSendMsgSize(maxSize*MB),
			// TODO
			grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
				opaReq, err := unaryOPARequest(ctx, req, info)
				log.Printf("interceptor opareq (err=%v): %+v", err, string(opaReq))
				return handler(ctx, req)
			}),
			// TODO
			grpc.StreamInterceptor(func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
				ctx := ss.Context()
				md, ok := metadata.FromIncomingContext(ctx)
				log.Printf("stream interceptor metadata (ok=%v): %+v", ok, md)
				log.Printf("stream interceptor info: %+v", info)
				return handler(srv, ss)
			}),
		)
		pb.RegisterEntroQServer(s, svc)
		hpb.RegisterHealthServer(s, health.NewServer())
		log.Printf("Starting EntroQ server %d -> mem", port)
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
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqmemsvc)")
	pflags.IntVar(&port, "port", 37706, "Port to listen on.")
	pflags.IntVar(&httpPort, "http_port", 9100, "Port to listen to HTTP requests on, including for /metrics.")
	pflags.IntVar(&maxSize, "max_size_mb", 10, "Maximum server message size (send and receive) in megabytes. If larger than 4MB, you must also set your gRPC client max size to take advantage of this.")

	viper.BindPFlag("port", pflags.Lookup("port"))
	viper.BindPFlag("http_port", pflags.Lookup("http_port"))
	viper.BindPFlag("max_size_mb", pflags.Lookup("max_size_mb"))
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
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqmemsvc.yml")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

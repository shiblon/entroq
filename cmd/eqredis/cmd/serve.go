package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/shiblon/entroq/pkg/authz/opahttp"
	"github.com/shiblon/entroq/pkg/backend/eqredis"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/eqsvcjson"
	"github.com/shiblon/entroq/pkg/otel"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const MB = 1024 * 1024

var serve struct {
	port     int
	httpPort int
	maxSize  int

	authzStrategy string
	opaURL        string
	opaPath       string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the EntroQ gRPC and HTTP/JSON service.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		resolveRedisFlags()

		var authzOpt eqsvcgrpc.Option
		switch serve.authzStrategy {
		case "opahttp":
			authzOpt = eqsvcgrpc.WithAuthorizer(opahttp.New(
				opahttp.WithHostURL(serve.opaURL),
				opahttp.WithAPIPath(serve.opaPath),
			))
		case "", "none":
			authzOpt = eqsvcgrpc.WithAuthorizer(nil)
		default:
			return fmt.Errorf("unknown authz strategy: %q", serve.authzStrategy)
		}

		mp, metricsHandler, stopMetrics, err := otel.NewPrometheusProvider()
		if err != nil {
			return fmt.Errorf("otel setup: %w", err)
		}
		defer stopMetrics()

		opener := eqredis.Opener(
			eqredis.WithAddr(redisAddr),
			eqredis.WithPassword(redisPwd),
			eqredis.WithRedisDB(redisDB),
		)

		svc, err := eqsvcgrpc.New(ctx, opener, authzOpt,
			eqsvcgrpc.WithMetricInterval(5*time.Second),
			eqsvcgrpc.WithMeterProvider(mp),
		)
		if err != nil {
			return fmt.Errorf("open eqredis backend: %w", err)
		}
		defer svc.Close()

		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", metricsHandler)
			path, handler, err := eqsvcjson.New(svc)
			if err != nil {
				log.Fatalf("create JSON/Connect handler: %v", err)
			}
			mux.Handle(path, handler)
			log.Fatalf("http server: %v", http.ListenAndServe(fmt.Sprintf(":%d", serve.httpPort), mux))
		}()

		lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", serve.port))
		if err != nil {
			return fmt.Errorf("listen on port %d: %w", serve.port, err)
		}

		s := grpc.NewServer(
			grpc.MaxRecvMsgSize(serve.maxSize*MB),
			grpc.MaxSendMsgSize(serve.maxSize*MB),
		)
		pb.RegisterEntroQServer(s, svc)
		hpb.RegisterHealthServer(s, health.NewServer())
		log.Printf("Starting EntroQ server %d -> redis(%s db=%d)", serve.port, redisAddr, redisDB)
		return s.Serve(lis)
	},
}

func init() {
	f := serveCmd.Flags()
	f.IntVar(&serve.port, "port", 37706, "gRPC service port.")
	f.IntVar(&serve.httpPort, "http_port", 9100, "HTTP port for /metrics and JSON/Connect API.")
	f.IntVar(&serve.maxSize, "max_size_mb", 10, "Maximum gRPC message size in MB (send and receive).")
	f.StringVar(&serve.authzStrategy, "authz", "none", "Authorization strategy: none, opahttp.")
	f.StringVar(&serve.opaURL, "opa_url", "", fmt.Sprintf("OPA base URL. Default: %s.", opahttp.DefaultHostURL))
	f.StringVar(&serve.opaPath, "opa_path", "", fmt.Sprintf("OPA API path. Default: %s.", opahttp.DefaultAPIPath))

	rootCmd.AddCommand(serveCmd)
}

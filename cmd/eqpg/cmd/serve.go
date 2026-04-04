package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shiblon/entroq/backend/eqpg"
	"github.com/shiblon/entroq/pkg/authz/opahttp"
	"github.com/shiblon/entroq/qsvc"
	"github.com/shiblon/entroq/qsvcjson"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"

	_ "github.com/lib/pq"
)

const MB = 1024 * 1024

var (
	port     int
	httpPort int
	maxSize  int
	attempts int

	authzStrategy string
	opaURL        string
	opaPath       string
	disableNotify bool
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the EntroQ gRPC and HTTP/JSON service.",
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
			return fmt.Errorf("unknown authz strategy: %q", authzStrategy)
		}

		resolveDBFlags()

		openerOptions := []eqpg.PGOpt{
			eqpg.WithDB(dbName),
			eqpg.WithUsername(dbUser),
			eqpg.WithPassword(dbPass),
			eqpg.WithConnectAttempts(attempts),
		}

		if disableNotify {
			openerOptions = append(openerOptions, eqpg.WithNotifyWaiter(nil))
		}

		opener := eqpg.Opener(dbAddr, openerOptions...)

		svc, err := qsvc.New(ctx, opener, authzOpt)
		if err != nil {
			return fmt.Errorf("failed to open pg backend: %w", err)
		}
		defer svc.Close()

		go func() {
			http.Handle("/metrics", promhttp.Handler())

			path, handler, err := qsvcjson.New(svc)
			if err != nil {
				log.Fatalf("failed to create JSON/Connect handler: %v", err)
			}
			http.Handle(path, handler)

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

func init() {
	flags := serveCmd.Flags()
	flags.IntVar(&port, "port", 37706, "gRPC service port.")
	flags.IntVar(&httpPort, "http_port", 9100, "HTTP port for /metrics and JSON/Connect API.")
	flags.IntVar(&attempts, "attempts", 10, "Connection attempts before dying (5-second pauses between tries).")
	flags.IntVar(&maxSize, "max_size_mb", 10, "Maximum gRPC message size in MB (send and receive).")
	flags.StringVar(&authzStrategy, "authz", "none", "Authorization strategy: none, opahttp.")
	flags.StringVar(&opaURL, "opa_url", "", fmt.Sprintf("OPA base URL (scheme://host:port). Default: %s.", opahttp.DefaultHostURL))
	flags.StringVar(&opaPath, "opa_path", "", fmt.Sprintf("OPA API path. Default: %s.", opahttp.DefaultAPIPath))
	flags.BoolVar(&disableNotify, "disable_notify_waiter", false, "Disable PostgreSQL LISTEN/NOTIFY completely. Required when using PgBouncer in transaction mode.")

	rootCmd.AddCommand(serveCmd)
}

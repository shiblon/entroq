package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/shiblon/entroq/pkg/authz/opahttp"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/eqsvcjson"
	"github.com/shiblon/entroq/pkg/otel"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	MB                = 1024 * 1024
	minSnapshotPeriod = time.Minute
)

var serve struct {
	port     int
	httpPort int
	maxSize  int

	authzStrategy string
	opaURL        string
	opaPath       string

	journal          string
	createJournalDir bool
	snapshotAndQuit  bool
	periodicSnapshot string
	journalMaxItems  int
	journalMaxBytes  int
	cleanup          bool
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the EntroQ gRPC and HTTP/JSON service.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		if serve.journal == "" {
			if serve.snapshotAndQuit || serve.cleanup || serve.periodicSnapshot != "" || serve.createJournalDir {
				return fmt.Errorf("journal settings given but no --journal directory specified")
			}
		}

		if serve.cleanup && !serve.snapshotAndQuit && serve.periodicSnapshot == "" {
			return fmt.Errorf("--journal_cleanup requires --snapshot_and_quit or --periodic_snapshot")
		}

		if serve.periodicSnapshot != "" && serve.snapshotAndQuit {
			return fmt.Errorf("--periodic_snapshot and --snapshot_and_quit are mutually exclusive")
		}

		if serve.createJournalDir {
			if err := os.MkdirAll(serve.journal, 0700); err != nil {
				return fmt.Errorf("create journal dir: %w", err)
			}
		}

		if serve.snapshotAndQuit {
			if err := eqmem.TakeSnapshot(ctx, serve.journal, serve.cleanup); err != nil {
				return fmt.Errorf("take snapshot in %q: %w", serve.journal, err)
			}
			return nil
		}

		if psf := serve.periodicSnapshot; psf != "" {
			period, err := time.ParseDuration(psf)
			if err != nil {
				return fmt.Errorf("periodic snapshot %q: not a valid duration: %w", psf, err)
			}
			if period < minSnapshotPeriod {
				log.Printf("Snapshot period %v smaller than %v: clamping", period, minSnapshotPeriod)
				period = minSnapshotPeriod
			}
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(period):
						if err := eqmem.TakeSnapshot(ctx, serve.journal, serve.cleanup); err != nil {
							log.Printf("Periodic snapshot %q: %v", serve.journal, err)
						}
					}
				}
			}()
		}

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

		svc, err := eqsvcgrpc.New(ctx, eqmem.Opener(
			eqmem.WithJournal(serve.journal),
			eqmem.WithMaxJournalBytes(int64(serve.journalMaxBytes)),
			eqmem.WithMaxJournalItems(serve.journalMaxItems),
			eqmem.WithMeterProvider(mp),
		), authzOpt, eqsvcgrpc.WithMeterProvider(mp))
		if err != nil {
			return fmt.Errorf("open eqmem backend: %w", err)
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
		log.Printf("Starting EntroQ server %d -> eqmem (journal=%q)", serve.port, serve.journal)
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
	f.StringVar(&serve.journal, "journal", "", "Journal directory for persistence. Default is ephemeral.")
	f.BoolVar(&serve.createJournalDir, "mkdir", false, "Create the journal directory if it does not exist.")
	f.BoolVar(&serve.snapshotAndQuit, "snapshot_and_quit", false, "Read the journal, write a snapshot, then exit. Requires --journal.")
	f.StringVar(&serve.periodicSnapshot, "periodic_snapshot", "", "Snapshot interval (e.g. 1h). Minimum 1m. Requires --journal.")
	f.BoolVar(&serve.cleanup, "journal_cleanup", false, "Remove compacted journal files after snapshotting. Requires --journal.")
	f.IntVar(&serve.journalMaxItems, "journal_max_items", 0, "Rotate journal after this many items (0 = default).")
	f.IntVar(&serve.journalMaxBytes, "journal_max_bytes", 0, "Rotate journal after this many bytes (0 = default).")

	rootCmd.AddCommand(serveCmd)
}

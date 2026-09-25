package cmd

import (
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/cmd/internal/eqserve"
	"github.com/shiblon/entroq/pkg/backend/eqpg"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/metric"

	_ "github.com/lib/pq"
)

var (
	serve eqserve.Config

	attempts          int
	readinessInterval time.Duration
	heartbeat         time.Duration
	noListen          bool
	initSchema        bool
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the EntroQ gRPC and HTTP/JSON service.",
	Long: `Serve a PostgreSQL-backed EntroQ over gRPC (--port, default 37706) and an
HTTP/JSON + Connect API (--http_port, default 9100, which also serves /metrics).

Requires an initialized schema at the version this build expects: run
"eqpg schema init" (or "eqpg schema upgrade"), or pass --init_schema to apply the
idempotent DDL before serving. The service refuses to start on a schema-version
mismatch rather than migrating a live database silently.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		dbTarget, connectionOptions := databaseConnection()

		// --heartbeat is the deprecated name for --readiness_interval.
		if cmd.Flags().Changed("heartbeat") && !cmd.Flags().Changed("readiness_interval") {
			readinessInterval = heartbeat
		}

		if initSchema {
			db, err := eqpg.OpenDB(dbTarget, connectionOptions...)
			if err != nil {
				return fmt.Errorf("schema init: open db: %w", err)
			}
			if err := eqpg.InitSchema(ctx, db); err != nil {
				db.Close()
				return fmt.Errorf("schema init: %w", err)
			}
			db.Close()
			log.Printf("Schema initialized at version %s.", eqpg.SchemaVersion)
		}

		return eqserve.Run(ctx, serve,
			func(mp metric.MeterProvider) entroq.BackendOpener {
				openerOptions := append(connectionOptions,
					eqpg.WithConnectAttempts(attempts),
					eqpg.WithReadinessInterval(readinessInterval),
					eqpg.WithMeterProvider(mp),
				)
				return eqpg.Opener(dbTarget, openerOptions...)
			},
			databaseDescription(dbTarget),
		)
	},
}

func init() {
	flags := serveCmd.Flags()
	serve.MetricInterval = 5 * time.Second
	serve.BindFlags(flags)
	flags.IntVar(&attempts, "attempts", 10, "Connection attempts before dying (5-second pauses between tries).")
	flags.DurationVar(&readinessInterval, "readiness_interval", eqpg.DefaultReadinessInterval,
		"Interval for notifying claims when tasks become ready through time, or through other database clients; non-positive disables.")
	flags.DurationVar(&heartbeat, "heartbeat", eqpg.DefaultReadinessInterval, "Deprecated name for --readiness_interval.")
	flags.MarkDeprecated("heartbeat", "use --readiness_interval")
	flags.BoolVar(&noListen, "no_listen", false, "Deprecated: the service no longer uses LISTEN/NOTIFY.")
	flags.MarkDeprecated("no_listen", "the service no longer uses LISTEN/NOTIFY; this flag does nothing")
	flags.BoolVar(&initSchema, "init_schema", false, "Initialize the EntroQ schema before serving (idempotent; safe to always set).")

	rootCmd.AddCommand(serveCmd)
}

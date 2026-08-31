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

	attempts   int
	heartbeat  time.Duration
	noListen   bool
	initSchema bool
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

		resolveDBFlags()

		if initSchema {
			db, err := eqpg.OpenDB(dbAddr,
				eqpg.WithDB(dbName),
				eqpg.WithUsername(dbUser),
				eqpg.WithPassword(dbPass),
			)
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
				openerOptions := []eqpg.PGOpt{
					eqpg.WithDB(dbName),
					eqpg.WithUsername(dbUser),
					eqpg.WithPassword(dbPass),
					eqpg.WithConnectAttempts(attempts),
					eqpg.WithHeartbeat(heartbeat),
					eqpg.WithMeterProvider(mp),
				}
				if noListen {
					openerOptions = append(openerOptions, eqpg.WithNoListen())
				}
				return eqpg.Opener(dbAddr, openerOptions...)
			},
			fmt.Sprintf("postgres(%s db=%s user=%s)", dbAddr, dbName, dbUser),
		)
	},
}

func init() {
	flags := serveCmd.Flags()
	serve.MetricInterval = 5 * time.Second
	serve.BindFlags(flags)
	flags.IntVar(&attempts, "attempts", 10, "Connection attempts before dying (5-second pauses between tries).")
	flags.DurationVar(&heartbeat, "heartbeat", 5*time.Second, "Interval at which this node triggers notifications for tasks that have become available (via NOTIFY).")
	flags.BoolVar(&noListen, "no_listen", false, "Disable the persistent PostgreSQL LISTEN connection; claims then fall back to polling. LISTEN is on by default for prompt claim wakeups via NOTIFY.")
	flags.BoolVar(&initSchema, "init_schema", false, "Initialize the EntroQ schema before serving (idempotent; safe to always set).")

	rootCmd.AddCommand(serveCmd)
}

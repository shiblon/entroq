package cmd

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/cmd/internal/eqserve"
	"github.com/shiblon/entroq/pkg/backend/eqsqlite"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/metric"
)

var serve eqserve.Config

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the experimental SQLite-backed EntroQ service.",
	Long: `Serve an experimental SQLite-backed EntroQ over gRPC (--port, default
37706) and an HTTP/JSON + Connect API (--http_port, default 9100, which also
serves /metrics).

The database uses WAL mode with synchronous=FULL and serializes writes through
one connection. Its schema and on-disk format may change without a migration
path while the backend remains experimental.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		resolveSQLiteFlags()
		if dbPath == "" {
			return fmt.Errorf("empty SQLite database path")
		}

		return eqserve.Run(cmd.Context(), serve,
			func(mp metric.MeterProvider) entroq.BackendOpener {
				return eqsqlite.Opener(dbPath, eqsqlite.WithMeterProvider(mp))
			},
			fmt.Sprintf("sqlite(%q)", dbPath),
		)
	},
}

func init() {
	serve.MetricInterval = 5 * time.Second
	serve.BindFlags(serveCmd.Flags())
	rootCmd.AddCommand(serveCmd)
}

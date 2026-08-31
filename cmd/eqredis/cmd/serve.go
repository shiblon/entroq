package cmd

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/cmd/internal/eqserve"
	"github.com/shiblon/entroq/pkg/backend/eqredis"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/metric"
)

var serve eqserve.Config

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the EntroQ gRPC and HTTP/JSON service.",
	Long: `Serve a Redis-backed EntroQ over gRPC (--port, default 37706) and an
HTTP/JSON + Connect API (--http_port, default 9100, which also serves /metrics).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		resolveRedisFlags()

		return eqserve.Run(cmd.Context(), serve,
			func(mp metric.MeterProvider) entroq.BackendOpener {
				return eqredis.Opener(
					eqredis.WithAddr(redisAddr),
					eqredis.WithPassword(redisPwd),
					eqredis.WithRedisDB(redisDB),
					eqredis.WithMeterProvider(mp),
				)
			},
			fmt.Sprintf("redis(%s db=%d)", redisAddr, redisDB),
		)
	},
}

func init() {
	f := serveCmd.Flags()
	serve.MetricInterval = 5 * time.Second
	serve.BindFlags(f)

	rootCmd.AddCommand(serveCmd)
}

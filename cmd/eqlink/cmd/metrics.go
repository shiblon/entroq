package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/shiblon/entroq/pkg/otel"
	"go.opentelemetry.io/otel/metric"
)

var metricsAddr string

// setupMetrics creates an OTel MeterProvider backed by the Prometheus exporter
// and starts a /metrics HTTP server on metricsAddr. The returned MeterProvider
// should be passed to Sender and ReceiverHandler. The stop function shuts down
// the metrics server and flushes the provider; call it on exit.
func setupMetrics(ctx context.Context) (metric.MeterProvider, func(), error) {
	mp, metricsHandler, stopProvider, err := otel.NewPrometheusProvider()
	if err != nil {
		return nil, nil, fmt.Errorf("otel setup: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler)
	srv := &http.Server{
		Addr:    metricsAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("metrics server listening on %s", metricsAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server: %v", err)
		}
	}()

	stop := func() {
		srv.Shutdown(ctx)
		stopProvider()
	}

	return mp, stop, nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&metricsAddr, "metrics_addr", ":9090", "Address to serve Prometheus metrics on.")
}

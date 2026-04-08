// Package otelutil provides helpers for setting up OpenTelemetry with a
// Prometheus exporter, used consistently across EntroQ services.
package otel

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// NewPrometheusProvider creates an OTel MeterProvider backed by a fresh
// Prometheus registry. The returned handler serves /metrics from that
// registry. Call stop on shutdown to flush the provider.
func NewPrometheusProvider() (metric.MeterProvider, http.Handler, func(), error) {
	registry := prometheus.NewRegistry()

	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

	stop := func() {
		mp.Shutdown(context.Background())
	}

	return mp, handler, stop, nil
}

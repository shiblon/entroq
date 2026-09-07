package eqsvcgrpc

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStatsMetricsIncludeQueuesAndNamespaces(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(ctx) }()

	svc, err := New(ctx, eqmem.Opener(), WithMeterProvider(mp))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	if _, err := svc.impl.Modify(ctx,
		entroq.InsertingInto("/metrics/jobs/inbox"),
		entroq.PuttingDocInto("/metrics/jobs/status", entroq.WithKeys("job-1", "state")),
	); err != nil {
		t.Fatalf("insert task and status doc: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	want := map[string]struct {
		label string
		value string
	}{
		"entroq.queue.size":     {label: "queue", value: "/metrics/jobs/inbox"},
		"entroq.namespace.size": {label: "doc_namespace", value: "/metrics/jobs/status"},
	}
	for name, target := range want {
		if !hasTotalGauge(rm, name, target.label, target.value, 1) {
			t.Errorf("metric %q has no total=1 point for %s=%q", name, target.label, target.value)
		}
	}
}

func hasTotalGauge(rm metricdata.ResourceMetrics, name, targetLabel, targetValue string, want float64) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[float64])
			if !ok {
				continue
			}
			for _, point := range gauge.DataPoints {
				typ, _ := point.Attributes.Value(attribute.Key("type"))
				target, _ := point.Attributes.Value(attribute.Key(targetLabel))
				if typ.AsString() == "total" && target.AsString() == targetValue && point.Value == want {
					return true
				}
			}
		}
	}
	return false
}

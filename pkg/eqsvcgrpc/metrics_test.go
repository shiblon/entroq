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
		if !hasGauge(rm, name, 1, map[string]string{
			"type":       "total",
			target.label: target.value,
		}) {
			t.Errorf("metric %q has no total=1 point for %s=%q", name, target.label, target.value)
		}
	}
}

func TestStatsMetricsFoldSessionComponents(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(ctx) }()

	svc, err := New(ctx, eqmem.Opener(), WithMeterProvider(mp))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	queueA := "/metrics/sess=a;gc=4102444800/inbox"
	queueB := "/metrics/gc=4102444800;sess=b/inbox"
	namespaceA := "/metrics/sess=a;gc=4102444800/status"
	namespaceB := "/metrics/gc=4102444800;sess=b/status"
	if _, err := svc.impl.Modify(ctx,
		entroq.InsertingInto(queueA),
		entroq.InsertingInto(queueB),
		entroq.InsertingInto(queueB),
		entroq.PuttingDocInto(namespaceA, entroq.WithKeys("job-a", "state")),
		entroq.PuttingDocInto(namespaceB, entroq.WithKeys("job-b", "state")),
	); err != nil {
		t.Fatalf("insert session-scoped tasks and docs: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	if !hasGauge(rm, "entroq.queue.size", 3, map[string]string{
		"type":  "total",
		"queue": "/metrics/*/inbox",
		"l1":    "/metrics",
		"l2":    "/metrics/*",
		"l3":    "/metrics/*/inbox",
	}) {
		t.Error("queue metric has no folded total=3 point")
	}
	if !hasGauge(rm, "entroq.namespace.size", 2, map[string]string{
		"type":          "total",
		"doc_namespace": "/metrics/*/status",
		"l1":            "/metrics",
		"l2":            "/metrics/*",
		"l3":            "/metrics/*/status",
	}) {
		t.Error("namespace metric has no folded total=2 point")
	}
	for _, raw := range []struct {
		metric string
		label  string
		value  string
	}{
		{"entroq.queue.size", "queue", queueA},
		{"entroq.queue.size", "queue", queueB},
		{"entroq.namespace.size", "doc_namespace", namespaceA},
		{"entroq.namespace.size", "doc_namespace", namespaceB},
	} {
		if hasGaugeTarget(rm, raw.metric, raw.label, raw.value) {
			t.Errorf("metric %q retains raw session label %s=%q", raw.metric, raw.label, raw.value)
		}
	}
}

func TestFoldQueueMetricStats(t *testing.T) {
	stats := map[string]*entroq.QueueStat{
		"/metrics/sess=a/inbox": {
			Size: 2, Claimed: 1, Available: 1, Future: 1, MaxClaims: 3,
		},
		"/metrics/gc=0;sess=b/inbox": {
			Size: 4, Claimed: 2, Available: 2, Future: 2, MaxClaims: 7,
		},
	}

	got := foldQueueMetricStats(stats)["/metrics/*/inbox"]
	if got == nil {
		t.Fatal("folded queue stat is absent")
	}
	if got.Name != "/metrics/*/inbox" || got.Size != 6 || got.Claimed != 3 ||
		got.Available != 3 || got.Future != 3 || got.MaxClaims != 7 {
		t.Errorf("folded queue stat = %+v, want summed counts and MaxClaims=7", got)
	}
}

func TestFoldNamespaceMetricStats(t *testing.T) {
	stats := map[string]*entroq.NamespaceStat{
		"/metrics/sess=a/status":      {Size: 2, Claimed: 1},
		"/metrics/gc=0;sess=b/status": {Size: 4, Claimed: 2},
	}

	got := foldNamespaceMetricStats(stats)["/metrics/*/status"]
	if got == nil {
		t.Fatal("folded namespace stat is absent")
	}
	if got.Name != "/metrics/*/status" || got.Size != 6 || got.Claimed != 3 {
		t.Errorf("folded namespace stat = %+v, want summed counts", got)
	}
}

func hasGauge(rm metricdata.ResourceMetrics, name string, wantValue float64, wantAttrs map[string]string) bool {
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
				if point.Value != wantValue {
					continue
				}
				matches := true
				for key, want := range wantAttrs {
					got, ok := point.Attributes.Value(attribute.Key(key))
					if !ok || got.AsString() != want {
						matches = false
						break
					}
				}
				if matches {
					return true
				}
			}
		}
	}
	return false
}

func hasGaugeTarget(rm metricdata.ResourceMetrics, name, targetLabel, targetValue string) bool {
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
				target, _ := point.Attributes.Value(attribute.Key(targetLabel))
				if target.AsString() == targetValue {
					return true
				}
			}
		}
	}
	return false
}

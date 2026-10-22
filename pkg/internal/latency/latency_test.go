package latency

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestBoundsAscend(t *testing.T) {
	if !slices.IsSorted(Bounds) || len(slices.Compact(slices.Clone(Bounds))) != len(Bounds) {
		t.Errorf("Bounds are not strictly ascending: %v", Bounds)
	}
}

// TestMillisecondsResolve records a 3ms latency, in seconds, and checks that
// it lands in a millisecond-wide bucket rather than the SDK default's first
// bucket, which holds everything under five seconds.
func TestMillisecondsResolve(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	h, err := mp.Meter("test").Float64Histogram("latency", metric.WithUnit("s"), Buckets())
	if err != nil {
		t.Fatal(err)
	}
	h.Record(ctx, 0.003)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	dp := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64]).DataPoints[0]
	i := slices.Index(dp.BucketCounts, 1)
	if i < 1 || i >= len(dp.Bounds) {
		t.Fatalf("3ms landed in bucket %d of bounds %v", i, dp.Bounds)
	}
	if low, high := dp.Bounds[i-1], dp.Bounds[i]; low != 0.0025 || high != 0.005 {
		t.Errorf("3ms landed in (%v, %v], want (0.0025, 0.005]", low, high)
	}
}

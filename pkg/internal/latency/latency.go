// Package latency holds the histogram buckets EntroQ's latency metrics share.
//
// Latencies are recorded in seconds, as OpenTelemetry recommends, but the
// SDK's default buckets (0, 5, 10, 25, ... 10000) are sized for
// milliseconds: in seconds, every latency under five lands in the first
// bucket, and a dashboard shows five-second resolution. Every latency
// histogram passes Buckets instead.
package latency

import "go.opentelemetry.io/otel/metric"

// Bounds are the bucket upper bounds, in seconds: from 100µs, for in-memory
// operations, to five minutes, for slow modifications, collection sweeps,
// round trips through a queue, and long HTTP sessions.
var Bounds = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05,
	0.1, 0.25, 0.5,
	1, 2.5, 5,
	10, 30, 60,
	120, 300,
}

// Buckets is the histogram option applying Bounds.
func Buckets() metric.HistogramOption {
	return metric.WithExplicitBucketBoundaries(Bounds...)
}

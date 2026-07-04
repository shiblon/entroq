// Package gcmetrics defines the OpenTelemetry instruments that every backend's
// garbage collector reports through. GC is a per-backend responsibility, but its
// telemetry should not be: keeping the metric names, units, and attributes in one
// place means a single Grafana dashboard covers eqpg, eqmem, and eqredis without
// per-backend drift.
//
// Each backend supplies its own Meter (entroq.pg, entroq.mem, entroq.redis), so
// the OTel instrumentation scope distinguishes which backend collected while the
// metric names stay identical. Attributes are the bounded queue hierarchy
// (l1/l2/l3 from queues.PathLabels) rather than the raw queue name, so metric
// cardinality is bounded by hierarchy depth, not by the number of queues.
package gcmetrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/shiblon/entroq/pkg/queues"
)

// Metrics holds the GC instruments for one backend. A nil *Metrics is a safe
// no-op, so callers that never wired a meter can report unconditionally.
type Metrics struct {
	deleted  metric.Int64Counter
	errors   metric.Int64Counter
	sweepDur metric.Float64Histogram
}

// New builds the shared GC instruments on the given meter. Pass a backend meter
// such as mp.Meter("entroq.pg"); pass a noop meter to disable reporting.
func New(meter metric.Meter) (*Metrics, error) {
	deleted, err := meter.Int64Counter("entroq.gc.deleted_total",
		metric.WithDescription("Total tasks deleted by garbage collection."),
	)
	if err != nil {
		return nil, fmt.Errorf("gc deleted counter: %w", err)
	}
	errors, err := meter.Int64Counter("entroq.gc.errors_total",
		metric.WithDescription("Total errors encountered during garbage collection."),
	)
	if err != nil {
		return nil, fmt.Errorf("gc errors counter: %w", err)
	}
	sweepDur, err := meter.Float64Histogram("entroq.gc.sweep_duration_seconds",
		metric.WithDescription("Duration of a full GC scan pass in seconds."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("gc sweep duration histogram: %w", err)
	}
	return &Metrics{deleted: deleted, errors: errors, sweepDur: sweepDur}, nil
}

// queueAttrs returns the bounded queue-hierarchy attributes used to group GC
// metrics. It omits the raw queue name on purpose: raw queue is unbounded
// cardinality, while l1/l2/l3 is bounded by hierarchy depth.
func queueAttrs(qname string) []attribute.KeyValue {
	l1, l2, l3 := queues.PathLabels(qname)
	return []attribute.KeyValue{
		attribute.String("l1", l1),
		attribute.String("l2", l2),
		attribute.String("l3", l3),
	}
}

// Deleted records n tasks collected from queue qname. n <= 0 and a nil receiver
// are no-ops.
func (m *Metrics) Deleted(ctx context.Context, qname string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.deleted.Add(ctx, int64(n), metric.WithAttributes(queueAttrs(qname)...))
}

// Error records one GC error of the given kind. qname may be empty when the
// error is not attributable to a single queue (e.g. a bulk collect). A nil
// receiver is a no-op.
func (m *Metrics) Error(ctx context.Context, qname, kind string) {
	if m == nil {
		return
	}
	attrs := append(queueAttrs(qname), attribute.String("kind", kind))
	m.errors.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// Sweep records the wall-clock duration of one GC pass. A nil receiver is a
// no-op.
func (m *Metrics) Sweep(ctx context.Context, d time.Duration) {
	if m == nil {
		return
	}
	m.sweepDur.Record(ctx, d.Seconds())
}

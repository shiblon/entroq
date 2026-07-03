// Package gc periodically collects tasks from queues that opt into garbage
// collection by naming convention. A queue whose name carries a /gc=<timestamp>
// component becomes eligible once that time passes; gc then drains its
// claimable tasks with TryClaim, which guarantees it
// never deletes a task a worker currently holds.
//
// Collection is best-effort: it obeys the timestamps it sees and uses only
// normal claim/modify machinery, so it is safe to run continuously beside live
// workers. It deliberately does not use the worker.Worker handler loop; it is a
// standalone periodic scan, optionally scoped by the standard queue matcher, so
// it works equally well embedded in a server or run on its own.
package gc

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/queues"
)

const (
	defaultInterval   = time.Minute
	defaultMaxSize    = 100
	maxBatchSize      = 1000
	defaultBatchPause = 100 * time.Millisecond
)

// Option configures RunLoop and Run.
type Option func(*config)

type config struct {
	match      []entroq.QueuesOpt
	interval   time.Duration
	maxSize    int
	batchPause time.Duration
	mp         metric.MeterProvider
}

func newConfig(opts []Option) config {
	c := config{
		interval:   defaultInterval,
		maxSize:    defaultMaxSize,
		batchPause: defaultBatchPause,
		mp:         noop.NewMeterProvider(),
	}
	for _, o := range opts {
		o(&c)
	}
	if c.maxSize <= 0 {
		c.maxSize = defaultMaxSize
	}
	if c.maxSize > maxBatchSize {
		c.maxSize = maxBatchSize
	}
	if c.batchPause < 0 {
		c.batchPause = 0
	}
	return c
}

// WithInterval sets how often the scan runs. Defaults to one minute.
func WithInterval(d time.Duration) Option {
	return func(c *config) {
		c.interval = d
	}
}

// WithMaxSize sets the maximum number of tasks GC claims and deletes per batch
// within a single queue before it pauses. Values above 1000 are capped;
// non-positive values fall back to the default of 100.
func WithMaxSize(n int) Option {
	return func(c *config) {
		c.maxSize = n
	}
}

// WithBatchPause sets how long GC rests between full batches while draining a
// single queue, so a large backlog does not monopolize the backend. The pause
// honors context cancellation. Defaults to 100ms; zero disables it.
func WithBatchPause(d time.Duration) Option {
	return func(c *config) {
		c.batchPause = d
	}
}

// WithMatch limits the scan to the queues selected by the standard EntroQ queue
// matcher (for example entroq.MatchPrefix or entroq.MatchExact). Options
// accumulate, matching the semantics of eq.Queues. With no match options every
// queue is scanned.
func WithMatch(opts ...entroq.QueuesOpt) Option {
	return func(c *config) {
		c.match = append(c.match, opts...)
	}
}

// WithMeterProvider sets the OTel MeterProvider used to emit metrics. Defaults
// to a no-op provider, so metrics are inert unless one is supplied.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.mp = mp
		}
	}
}

// metrics holds the OTel instruments emitted by a scan.
type metrics struct {
	deleted  metric.Int64Counter
	errors   metric.Int64Counter
	sweepDur metric.Float64Histogram
}

func newMetrics(mp metric.MeterProvider) (*metrics, error) {
	m := mp.Meter("entroq.gc")
	deleted, err := m.Int64Counter("entroq.gc.deleted_total",
		metric.WithDescription("Total tasks deleted by garbage collection."),
	)
	if err != nil {
		return nil, fmt.Errorf("gc deleted counter: %w", err)
	}
	errors, err := m.Int64Counter("entroq.gc.errors_total",
		metric.WithDescription("Total errors encountered during garbage collection."),
	)
	if err != nil {
		return nil, fmt.Errorf("gc errors counter: %w", err)
	}
	sweepDur, err := m.Float64Histogram("entroq.gc.sweep_duration_seconds",
		metric.WithDescription("Duration of a full GC scan pass in seconds."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("gc sweep duration histogram: %w", err)
	}
	return &metrics{deleted: deleted, errors: errors, sweepDur: sweepDur}, nil
}

// queueAttrs returns a fresh attribute set identifying a queue and its
// hierarchy, matching the labels used by the queue-size gauge so GC metrics
// filter under the same dashboard variables.
func queueAttrs(qname string) []attribute.KeyValue {
	l1, l2, l3 := queues.PathLabels(qname)
	return []attribute.KeyValue{
		attribute.String("queue", qname),
		attribute.String("l1", l1),
		attribute.String("l2", l2),
		attribute.String("l3", l3),
	}
}

func (m *metrics) addError(ctx context.Context, qname, kind string) {
	// queueAttrs allocates a fresh slice, so appending here never aliases.
	attrs := append(queueAttrs(qname), attribute.String("kind", kind))
	m.errors.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RunLoop runs a scan periodically until ctx is canceled. Scan errors are
// logged and do not stop the loop.
func RunLoop(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	c := newConfig(opts)
	m, err := newMetrics(c.mp)
	if err != nil {
		return fmt.Errorf("gc metrics: %w", err)
	}
	t := time.NewTicker(c.interval)
	defer t.Stop()
	log.Printf("GC started: interval=%v", c.interval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := scan(ctx, eq, c, m); err != nil {
				log.Printf("gc scan error: %v", err)
			}
		}
	}
}

// Run performs one scan pass, deleting tasks from queues whose gc activation
// time has passed. It uses TryClaim to drain each queue, which
// provides mutual exclusion between concurrent GC instances and guarantees it
// only deletes tasks no worker currently holds. Use WithMatch to limit the
// queues scanned; with none, every queue is scanned.
func Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	c := newConfig(opts)
	m, err := newMetrics(c.mp)
	if err != nil {
		return fmt.Errorf("gc metrics: %w", err)
	}
	return scan(ctx, eq, c, m)
}

func scan(ctx context.Context, eq *entroq.EntroQ, c config, m *metrics) error {
	start := time.Now()
	defer func() { m.sweepDur.Record(ctx, time.Since(start).Seconds()) }()

	now := time.Now()

	queueMap, err := eq.Queues(ctx, c.match...)
	if err != nil {
		return fmt.Errorf("list queues: %w", err)
	}

	var cleaned, skipped int
	for qname := range queueMap {
		activateAt, present, err := queues.GCActivation(qname)
		if !present {
			continue
		}
		if err != nil {
			// A malformed activation value must never be treated as "collect".
			log.Printf("gc activation %s: %v", qname, err)
			m.addError(ctx, qname, "parse")
			continue
		}
		if now.Before(activateAt) {
			skipped++
			continue
		}

		if drainQueue(ctx, eq, c, m, qname) {
			cleaned++
		}
	}

	if cleaned > 0 || skipped > 0 {
		log.Printf("gc scan: %d queues cleaned, %d not yet due", cleaned, skipped)
	}

	return nil
}

// drainQueue claims and deletes the claimable tasks in qname in batches of at
// most c.maxSize, committing each batch in a single Modify. Every task in a
// batch is held by this claimant, so the atomic delete cannot be tripped by a
// stale version. Between full batches it rests for c.batchPause so a large
// backlog does not monopolize the backend. It returns true if it finished
// without a claim or delete error.
func drainQueue(ctx context.Context, eq *entroq.EntroQ, c config, m *metrics, qname string) bool {
	for {
		var batch []entroq.ModifyArg
		claimErr := false
		for len(batch) < c.maxSize {
			task, err := eq.TryClaim(ctx, entroq.From(qname))
			if err != nil {
				log.Printf("gc claim %s: %v", qname, err)
				m.addError(ctx, qname, "claim")
				claimErr = true
				break
			}
			if task == nil {
				break
			}
			batch = append(batch, task.Delete())
		}

		// Delete whatever we managed to claim, even if claiming then errored;
		// we hold those leases and abandoning them just delays their cleanup.
		if len(batch) > 0 {
			if _, err := eq.Modify(ctx, batch...); err != nil {
				log.Printf("gc delete batch in %s: %v", qname, err)
				m.addError(ctx, qname, "delete")
				return false
			}
			m.deleted.Add(ctx, int64(len(batch)), metric.WithAttributes(queueAttrs(qname)...))
		}

		if claimErr {
			return false
		}
		// A partial batch means the queue is drained; a full one may have left
		// more behind, so rest and continue.
		if len(batch) < c.maxSize {
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(c.batchPause):
		}
	}
}

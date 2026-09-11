package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type workerState string

const (
	workerIdle workerState = "idle"
	workerBusy workerState = "busy"
)

type workerSlotState struct {
	state workerState
	since time.Time
}

// Task outcomes recorded by entroq.worker.tasks_total.
const (
	outcomeDone    = "done"
	outcomeRetried = "retried"
	outcomeMoved   = "moved"
	outcomeFailed  = "failed"
)

type workerMetrics struct {
	sync.Mutex
	next  uint64
	slots map[uint64]workerSlotState
	tasks metric.Int64Counter
}

type workerSlot struct {
	metrics *workerMetrics
	id      uint64
}

func newWorkerMetrics(mp metric.MeterProvider) (*workerMetrics, error) {
	meter := mp.Meter("entroq.worker")
	slots, err := meter.Int64ObservableGauge("entroq.worker.slots",
		metric.WithDescription("Current worker execution slots by state."),
	)
	if err != nil {
		return nil, fmt.Errorf("worker slots gauge: %w", err)
	}
	maxDuration, err := meter.Float64ObservableGauge("entroq.worker.state.max_duration",
		metric.WithDescription("Longest current worker slot state duration."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("worker state max duration gauge: %w", err)
	}

	// Throughput per worker. EntroQ deletes a task when it completes, so no
	// record of who handled what survives in the queue; this counter is the only
	// place that history exists, and it is per-process.
	//
	// The claimant attribute is the worker's client ID. It defaults to a random
	// value per client, which means a new time series on every restart. A
	// deployment that wants stable per-worker series should set it to something
	// durable, such as the pod name, with entroq.WithClaimantID.
	tasks, err := meter.Int64Counter("entroq.worker.tasks_total",
		metric.WithDescription("Tasks handled by this worker, by queue, claimant, and outcome."),
	)
	if err != nil {
		return nil, fmt.Errorf("worker tasks counter: %w", err)
	}

	metrics := &workerMetrics{slots: make(map[uint64]workerSlotState), tasks: tasks}
	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		counts, maxima := metrics.snapshot(time.Now())
		for _, state := range []workerState{workerIdle, workerBusy} {
			attrs := metric.WithAttributes(attribute.String("state", string(state)))
			observer.ObserveInt64(slots, counts[state], attrs)
			observer.ObserveFloat64(maxDuration, maxima[state].Seconds(), attrs)
		}
		return nil
	}, slots, maxDuration)
	if err != nil {
		return nil, fmt.Errorf("worker metrics callback: %w", err)
	}
	return metrics, nil
}

func (m *workerMetrics) add() *workerSlot {
	if m == nil {
		return nil
	}
	m.Lock()
	defer m.Unlock()
	m.next++
	m.slots[m.next] = workerSlotState{state: workerIdle, since: time.Now()}
	return &workerSlot{metrics: m, id: m.next}
}

func (s *workerSlot) set(state workerState) {
	if s == nil {
		return
	}
	s.metrics.Lock()
	defer s.metrics.Unlock()
	current, ok := s.metrics.slots[s.id]
	if !ok || current.state == state {
		return
	}
	s.metrics.slots[s.id] = workerSlotState{state: state, since: time.Now()}
}

func (s *workerSlot) remove() {
	if s == nil {
		return
	}
	s.metrics.Lock()
	defer s.metrics.Unlock()
	delete(s.metrics.slots, s.id)
}

func (m *workerMetrics) snapshot(now time.Time) (map[workerState]int64, map[workerState]time.Duration) {
	counts := map[workerState]int64{workerIdle: 0, workerBusy: 0}
	maxima := map[workerState]time.Duration{workerIdle: 0, workerBusy: 0}
	m.Lock()
	defer m.Unlock()
	for _, slot := range m.slots {
		counts[slot.state]++
		if elapsed := now.Sub(slot.since); elapsed > maxima[slot.state] {
			maxima[slot.state] = elapsed
		}
	}
	return counts, maxima
}

// recordTask counts one finished task attempt. It is a no-op when metrics are
// not configured, so an uninstrumented worker pays nothing.
func (m *workerMetrics) recordTask(ctx context.Context, queue, claimant, outcome string) {
	if m == nil || m.tasks == nil {
		return
	}
	m.tasks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", queue),
		attribute.String("claimant", claimant),
		attribute.String("outcome", outcome),
	))
}

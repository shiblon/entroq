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

type workerMetrics struct {
	sync.Mutex
	next  uint64
	slots map[uint64]workerSlotState
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

	metrics := &workerMetrics{slots: make(map[uint64]workerSlotState)}
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

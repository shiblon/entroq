package eqmem

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestGCUnderConcurrentLoad runs the GC loop at a tight interval against a live
// concurrent workload (producers inserting into gc= queues, consumers
// claiming/deleting from work queues) to shake out data races between GC and
// normal operations. Meant to be run with -race.
func TestGCUnderConcurrentLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, Opener(withGCInterval(time.Millisecond)))
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer client.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Producers: continuously insert immediately-collectable tasks into gc=0
	// queues, racing the GC loop that is draining them.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := fmt.Sprintf("/loadtest/p%d/gc=0", i)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithRawValue([]byte("{}")))); err != nil {
					return
				}
			}
		}(i)
	}

	// Consumers: claim and delete from non-gc work queues, exercising the normal
	// claim/modify paths concurrently with GC.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wq := fmt.Sprintf("/loadtest/work%d", i)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := client.Modify(ctx, entroq.InsertingInto(wq, entroq.WithRawValue([]byte("{}")))); err != nil {
					return
				}
				task, err := client.TryClaim(ctx, entroq.From(wq), entroq.ClaimFor(time.Second))
				if err != nil {
					return
				}
				if task != nil {
					if _, err := client.Modify(ctx, task.Delete()); err != nil {
						continue
					}
				}
			}
		}(i)
	}

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestGCMetricsEmitted pins the GC telemetry contract the Grafana dashboard
// depends on: collecting reports entroq.gc.deleted_total under the entroq.mem
// meter, attributed by the bounded queue hierarchy (l1). Driven white-box via
// collectOnce so the counts are exact.
func TestGCMetricsEmitted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	b, err := New(ctx, WithMeterProvider(mp), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer b.Close()

	// Three due tasks across two queues that share an l1 of "/metrics".
	past := time.Now().Add(-time.Hour)
	inserts := []struct{ id, queue string }{
		{"m1", "/metrics/a/gc=0"},
		{"m2", "/metrics/a/gc=0"},
		{"m3", "/metrics/b/gc=0"},
	}
	for _, c := range inserts {
		if _, err := b.Modify(ctx, entroq.NewModification("",
			entroq.InsertingInto(c.queue, entroq.WithID(c.id), entroq.WithArrivalTime(past), entroq.WithRawValue([]byte("{}"))))); err != nil {
			t.Fatalf("insert %s: %v", c.id, err)
		}
	}

	if n, err := b.collectOnce(ctx, 100); err != nil || n != 3 {
		t.Fatalf("collectOnce = (%d, %v), want (3, nil)", n, err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	var total int64
	found := false
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "entroq.mem" {
			continue
		}
		for _, md := range sm.Metrics {
			if md.Name != "entroq.gc.deleted_total" {
				continue
			}
			found = true
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("deleted_total data is %T, want Sum[int64]", md.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
				l1, ok := dp.Attributes.Value(attribute.Key("l1"))
				if !ok {
					t.Errorf("data point missing l1 attribute: %v", dp.Attributes.ToSlice())
				} else if l1.AsString() != "/metrics" {
					t.Errorf("l1 = %q, want %q", l1.AsString(), "/metrics")
				}
			}
		}
	}
	if !found {
		t.Fatal("entroq.gc.deleted_total not reported under the entroq.mem meter")
	}
	if total != 3 {
		t.Errorf("deleted_total sum = %d, want 3", total)
	}
}

// TestGCReportsMalformed pins the loud-on-misconfiguration behavior: a queue that
// opts into GC with an unparseable gc= value is never collected (would otherwise
// pile up silently) but is surfaced via entroq.gc.errors_total{kind=malformed}.
func TestGCReportsMalformed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	b, err := New(ctx, WithMeterProvider(mp), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer b.Close()

	// A task in a malformed gc= queue: opted in, but the value won't parse.
	past := time.Now().Add(-time.Hour)
	const bad = "/bad/gc=notatime"
	if _, err := b.Modify(ctx, entroq.NewModification("",
		entroq.InsertingInto(bad, entroq.WithID("x1"), entroq.WithArrivalTime(past), entroq.WithRawValue([]byte("{}"))))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	b.reportMalformed(ctx)

	// It must NOT be collected.
	if n, err := b.collectOnce(ctx, 100); err != nil || n != 0 {
		t.Fatalf("collectOnce = (%d, %v), want (0, nil): malformed queue must not be collected", n, err)
	}
	if got, err := b.Tasks(ctx, &entroq.TasksQuery{Queue: bad}); err != nil || len(got) != 1 {
		t.Fatalf("malformed queue task count = %d (err %v), want 1 (survives)", len(got), err)
	}

	// It must be reported via errors_total{kind=malformed}.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var malformed int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "entroq.mem" {
			continue
		}
		for _, md := range sm.Metrics {
			if md.Name != "entroq.gc.errors_total" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("errors_total data is %T, want Sum[int64]", md.Data)
			}
			for _, dp := range sum.DataPoints {
				if kind, ok := dp.Attributes.Value(attribute.Key("kind")); ok && kind.AsString() == "malformed" {
					malformed += dp.Value
				}
			}
		}
	}
	if malformed < 1 {
		t.Errorf("errors_total{kind=malformed} = %d, want >= 1", malformed)
	}
}

// TestGCLoopCollects asserts the always-on backend GC loop actually RUNS: built
// with a short interval, it auto-collects a due gc= task with no manual trigger.
func TestGCLoopCollects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, Opener(withGCInterval(20*time.Millisecond)))
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer client.Close()

	eqtest.GCCollectsInLoop(ctx, t, client, "/test/gcloop")
}

// TestGCCollectOnce drives collectOnce directly (white-box) to pin the semantics:
// due gc= tasks are collected; not-yet-due activation, future arrival
// (claimed-equivalent), and non-gc queues are left alone. The backend is a fresh,
// isolated in-memory store, so the deleted count is exact. The GC interval is set
// long so the background loop does not collect out from under the assertions.
func TestGCCollectOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := New(ctx, withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer b.Close()

	const p = "/test/collectonce"
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	futureGC := fmt.Sprintf("%s/c/gc=%d", p, future.Unix())

	cases := []struct {
		id, queue string
		at        time.Time
		collected bool
	}{
		{"co_due0", p + "/a/gc=0", past, true},       // always-active, arrived => collected
		{"co_past", p + "/b/gc=100", past, true},     // activation long past, arrived => collected
		{"co_future", futureGC, past, false},         // activation in the future => not due
		{"co_claimed", p + "/a/gc=0", future, false}, // arrival in the future => not collectable
		{"co_plain", p + "/plain", past, false},      // not a gc= queue
	}
	for _, c := range cases {
		if _, err := b.Modify(ctx, entroq.NewModification("",
			entroq.InsertingInto(c.queue, entroq.WithID(c.id), entroq.WithArrivalTime(c.at), entroq.WithRawValue([]byte("{}"))))); err != nil {
			t.Fatalf("insert %s: %v", c.id, err)
		}
	}

	n, err := b.collectOnce(ctx, 100)
	if err != nil {
		t.Fatalf("collectOnce: %v", err)
	}
	if want := 2; n != want {
		t.Errorf("collectOnce deleted %d, want %d (co_due0, co_past)", n, want)
	}

	for _, c := range cases {
		got, err := b.Tasks(ctx, &entroq.TasksQuery{Queue: c.queue})
		if err != nil {
			t.Fatalf("tasks %q: %v", c.queue, err)
		}
		present := false
		for _, tk := range got {
			if tk.ID == c.id {
				present = true
			}
		}
		if c.collected && present {
			t.Errorf("%s should have been collected but is still present", c.id)
		}
		if !c.collected && !present {
			t.Errorf("%s should have survived but was collected", c.id)
		}
	}
}

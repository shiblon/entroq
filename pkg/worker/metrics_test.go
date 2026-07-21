package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type workerMetricSnapshot struct {
	idle, busy                 int64
	maxIdleSeconds, maxBusySec float64
}

func collectWorkerMetrics(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) workerMetricSnapshot {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect worker metrics: %v", err)
	}
	var snapshot workerMetricSnapshot
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "entroq.worker" {
			continue
		}
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Gauge[int64]:
				if m.Name != "entroq.worker.slots" {
					continue
				}
				for _, point := range data.DataPoints {
					state, _ := point.Attributes.Value(attribute.Key("state"))
					if state.AsString() == "idle" {
						snapshot.idle = point.Value
					} else if state.AsString() == "busy" {
						snapshot.busy = point.Value
					}
				}
			case metricdata.Gauge[float64]:
				if m.Name != "entroq.worker.state.max_duration" {
					continue
				}
				for _, point := range data.DataPoints {
					state, _ := point.Attributes.Value(attribute.Key("state"))
					if state.AsString() == "idle" {
						snapshot.maxIdleSeconds = point.Value
					} else if state.AsString() == "busy" {
						snapshot.maxBusySec = point.Value
					}
				}
			}
		}
	}
	return snapshot
}

func waitWorkerMetrics(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, idle, busy int64) workerMetricSnapshot {
	t.Helper()
	for {
		snapshot := collectWorkerMetrics(ctx, t, reader)
		if snapshot.idle == idle && snapshot.busy == busy {
			return snapshot
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker metrics never reached idle=%d busy=%d", idle, busy)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestWorkerMetricsTrackConcurrentRunSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	w := New(client,
		WithMeterProvider[string](mp),
		WithDoModify(func(_ context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) (*Result, error) {
			started <- struct{}{}
			<-release
			return Modify(task.Delete()), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- w.Run(runCtx, Watching("worker_metrics")) }()
	}
	idle := waitWorkerMetrics(ctx, t, reader, 2, 0)
	if idle.maxIdleSeconds < 0 {
		t.Errorf("max idle duration = %v, want non-negative", idle.maxIdleSeconds)
	}

	if _, err := client.Modify(ctx, entroq.InsertingInto("worker_metrics", entroq.WithValue("work"))); err != nil {
		t.Fatalf("insert: %v", err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker never started task")
	}
	time.Sleep(10 * time.Millisecond)
	mixed := waitWorkerMetrics(ctx, t, reader, 1, 1)
	if mixed.maxIdleSeconds <= 0 || mixed.maxBusySec <= 0 {
		t.Errorf("state durations = idle %v busy %v, want both positive", mixed.maxIdleSeconds, mixed.maxBusySec)
	}

	close(release)
	waitWorkerMetrics(ctx, t, reader, 2, 0)
	runCancel()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("worker run: %v", err)
		}
	}
	waitWorkerMetrics(ctx, t, reader, 0, 0)
}

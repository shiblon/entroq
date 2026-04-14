package async_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// TestSharedEQSidecar verifies the basic sidecar pattern: sender and receiver
// share one EQ instance. A request inserted by the sender arrives at the
// receiver and a response comes back. This is the single-datacenter (or
// WAN-accessible shared EQ) scenario.
func TestSharedEQSidecar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	upstream := startUpstream()
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()

	const queue = "svc-test"
	stopReceivers := mustStartReceivers(ctx, t, eq, queue, upstream.URL, 2)
	defer stopReceivers()

	sender := async.NewSender(eq, "", queue)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://test/%s/ping", queue), nil).WithContext(ctx)
	w := httptest.NewRecorder()
	sender.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (body: %s)", w.Code, w.Body)
	}
}

// TestForwarder verifies the Forwarder type: tasks inserted into the source
// queue arrive in the destination queue on a separate EQ instance, and the
// source queue is drained.
func TestForwarder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eqA, stopA := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopA()

	eqB, stopB := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopB()

	const (
		srcQueue = "fwd-src"
		dstQueue = "fwd-dst"
		n        = 5
	)

	fwdCtx, fwdCancel := context.WithCancel(ctx)
	defer fwdCancel()

	fwd := async.NewForwarder(eqA, eqB, srcQueue,
		async.WithForwarderDestQueue(dstQueue),
		async.WithForwarderConcurrency(2),
	)
	go fwd.Run(fwdCtx) //nolint:errcheck

	for i := range n {
		if _, err := eqA.Modify(ctx, entroq.InsertingInto(srcQueue, entroq.WithValue(i))); err != nil {
			t.Fatalf("insert task %d: %v", i, err)
		}
	}

	// Wait until all n tasks have arrived in eqB.
	for {
		ts, err := eqB.Tasks(ctx, dstQueue)
		if err != nil {
			t.Fatalf("tasks on eqB: %v", err)
		}
		if len(ts) == n {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timeout: only %d/%d tasks arrived in dst", len(ts), n)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Stop the forwarder and verify eqA is drained.
	fwdCancel()
	ts, err := eqA.Tasks(context.Background(), srcQueue)
	if err != nil {
		t.Fatalf("eqA tasks: %v", err)
	}
	if len(ts) != 0 {
		t.Errorf("eqA src: got %d tasks, want 0", len(ts))
	}
}

// TestCrossInstanceForwarding verifies the two-EQ-instance pattern:
// a forwarder worker bridges tasks from eqA to eqB, giving each datacenter
// its own EQ while still delivering work across the boundary.
//
// The forwarder provides at-least-once delivery: if it crashes after inserting
// into eqB but before deleting from eqA, the task will be re-forwarded on
// the next claim. Receiving workers on eqB should be idempotent.
func TestCrossInstanceForwarding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eqA, stopA := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopA()

	eqB, stopB := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopB()

	const (
		outbox = "dc-b/inbox"
		inbox  = "dc-b/inbox"
		n      = 5
	)

	// Forwarder: claims from eqA's outbox, inserts into eqB's inbox, then
	// deletes from eqA. At-least-once: insert to eqB and delete from eqA are
	// separate transactions.
	fwdCtx, fwdCancel := context.WithCancel(ctx)
	defer fwdCancel()

	fwd := worker.New(eqA,
		worker.WithDoWork(func(ctx context.Context, _ *entroq.Task, val json.RawMessage, _ []*entroq.Doc) error {
			_, err := eqB.Modify(ctx, entroq.InsertingInto(inbox, entroq.WithRawValue(val)))
			return err
		}),
		worker.WithFinish(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
			_, err := eqA.Modify(ctx, task.Delete())
			return err
		}),
	)
	go fwd.Run(fwdCtx, worker.Watching(outbox)) //nolint:errcheck

	// Insert tasks into eqA.
	for i := range n {
		if _, err := eqA.Modify(ctx, entroq.InsertingInto(outbox, entroq.WithValue(i))); err != nil {
			t.Fatalf("insert task %d: %v", i, err)
		}
	}

	// Wait until all n tasks have arrived in eqB.
	for {
		ts, err := eqB.Tasks(ctx, inbox)
		if err != nil {
			t.Fatalf("tasks on eqB: %v", err)
		}
		if len(ts) == n {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timeout: only %d/%d tasks arrived in eqB", len(ts), n)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// eqA outbox should now be empty.
	fwdCancel()
	ts, err := eqA.Tasks(context.Background(), outbox)
	if err != nil {
		t.Fatalf("eqA tasks: %v", err)
	}
	if len(ts) != 0 {
		t.Errorf("eqA outbox: got %d tasks, want 0", len(ts))
	}
}

package pullworker

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/gc"
	"github.com/shiblon/entroq/pkg/queues"
)

// TestTombstoneQueueGCEligible confirms the default tombstone queue opts into
// server-side GC via its name, so a running server reaps its orphans without a
// separate reaper.
func TestTombstoneQueueGCEligible(t *testing.T) {
	q := TombstoneQueue("/svc/inbox")
	at, present, err := queues.GCActivation(q)
	if err != nil {
		t.Fatalf("GCActivation(%q): %v", q, err)
	}
	if !present {
		t.Fatalf("tombstone queue %q is not GC-eligible; the server would never reap it", q)
	}
	if at.After(time.Now()) {
		t.Errorf("tombstone queue %q should be collectable now (gc=0), got activate-at %v", q, at)
	}
}

// TestTombstonesReapedByGC confirms that an orphaned tombstone -- one whose TTL
// (arrival time) has elapsed -- is deleted by the GC pass the destination server
// runs, so no separate reaper is needed. It also implicitly checks that the
// tombstone is left alone until its TTL, since GC only claims arrived tasks.
func TestTombstonesReapedByGC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dst := memClient(ctx, t)

	tq := TombstoneQueue("/svc/inbox")
	if _, err := dst.Modify(ctx, entroq.InsertingInto(tq,
		entroq.WithID("xfer-orphan"),
		entroq.WithArrivalTimeIn(20*time.Millisecond),
	)); err != nil {
		t.Fatalf("seed orphan tombstone: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := gc.Run(ctx, dst); err != nil {
			t.Fatalf("gc run: %v", err)
		}
		sizes, err := dst.Queues(ctx)
		if err != nil {
			t.Fatalf("Queues: %v", err)
		}
		if sizes[tq] == 0 {
			return // reaped once its arrival time passed
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("orphan tombstone in %q not reaped by GC (size %d)", tq, sizes[tq])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

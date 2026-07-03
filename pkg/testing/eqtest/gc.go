package eqtest

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// GCCollectsInLoop verifies that a backend garbage-collects on its own: a task
// in a gc=-marked queue, arrived and due, is deleted by the backend's background
// loop with no explicit trigger. This asserts the ambient behavior -- that
// constructing a backend starts and runs the GC loop -- not merely that a
// collection pass is callable. Backends run this with a short GC interval (an
// internal, test-only knob) so the loop fires promptly.
func GCCollectsInLoop(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()

	// gc=0 => GC always active for this queue; default arrival (now) => due now.
	queue := path.Join(qPrefix, "gcloop", "gc=0")
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithRawValue([]byte("{}")))); err != nil {
		t.Fatalf("GCCollectsInLoop: insert into %q: %v", queue, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		sizes, err := client.Queues(ctx, entroq.MatchExact(queue))
		if err != nil {
			t.Fatalf("GCCollectsInLoop: queues: %v", err)
		}
		if sizes[queue] == 0 {
			return // collected by the background loop
		}
		if time.Now().After(deadline) {
			t.Fatalf("GCCollectsInLoop: %q not collected by the background loop within deadline (size %d)", queue, sizes[queue])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

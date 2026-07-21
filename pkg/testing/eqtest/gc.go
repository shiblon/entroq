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

// GCDocGroups verifies the doc-GC safety boundary: one live lease protects the
// entire primary-key group, and once released the whole group is collected.
func GCDocGroups(ctx context.Context, t *testing.T, backend entroq.Backend, collect func(context.Context, int) (int, error), prefix string) {
	t.Helper()
	ns := path.Join(prefix, "doc-gc-ns")
	activation := time.Now().Add(2 * time.Second)
	key := path.Join(prefix, "doc-gc", "gc="+activation.UTC().Format(time.RFC3339Nano))
	past := time.Now().Add(-time.Hour)

	if _, err := backend.Modify(ctx, entroq.NewModification("",
		entroq.PuttingDocInto(ns, entroq.WithIDKeys(entroq.GenHex16(), key, "a")),
		entroq.PuttingDocInto(ns, entroq.WithIDKeys(entroq.GenHex16(), key, "b")),
	)); err != nil {
		t.Fatalf("GCDocGroups: insert: %v", err)
	}
	claimed, err := backend.ClaimDocs(ctx, &entroq.DocClaim{
		Namespace: ns,
		Key:       key,
		Claimant:  "doc-gc-test",
		Duration:  time.Hour,
	})
	if err != nil {
		t.Fatalf("GCDocGroups: claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("GCDocGroups: claimed %d docs, want 2", len(claimed))
	}
	if wait := time.Until(activation); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			t.Fatalf("GCDocGroups: wait for activation: %v", ctx.Err())
		case <-timer.C:
		}
	}

	if _, err := collect(ctx, 10000); err != nil {
		t.Fatalf("GCDocGroups: collect claimed group: %v", err)
	}
	if docs, err := backend.Docs(ctx, &entroq.DocQuery{Namespace: ns, KeyExact: key}); err != nil || len(docs) != 2 {
		t.Fatalf("GCDocGroups: claimed group changed: len=%d err=%v", len(docs), err)
	}

	args := make([]entroq.ModifyArg, 0, len(claimed))
	for _, doc := range claimed {
		args = append(args, doc.Change(entroq.WithDocArrivalTime(past)))
	}
	if _, err := backend.Modify(ctx, entroq.NewModification("doc-gc-test", args...)); err != nil {
		t.Fatalf("GCDocGroups: release: %v", err)
	}
	if _, err := collect(ctx, 10000); err != nil {
		t.Fatalf("GCDocGroups: collect released group: %v", err)
	}
	if docs, err := backend.Docs(ctx, &entroq.DocQuery{Namespace: ns, KeyExact: key}); err != nil || len(docs) != 0 {
		t.Fatalf("GCDocGroups: released group remains: len=%d err=%v", len(docs), err)
	}
}

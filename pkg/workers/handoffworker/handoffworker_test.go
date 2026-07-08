package handoffworker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"golang.org/x/sync/errgroup"
)

// memClient opens an independent in-memory EntroQ instance. Two of these stand
// in for two separate instances (source and destination).
func memClient(ctx context.Context, t *testing.T) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open mem client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
}

// TestPullDelivers covers the happy path: a source task is delivered into the
// destination inbox, the source task is deleted, and the worker eager-cleans the
// tombstone it created (so nothing is left for the reaper).
func TestPullDelivers(t *testing.T) {
	// A deadline turns any "never makes progress" regression into a localized
	// failure here instead of hanging the whole test binary on WaitQueuesEmpty.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := memClient(ctx, t)
	dst := memClient(ctx, t)

	const (
		outbox = "/out"
		inbox  = "/in"
	)
	want := `"hello"`
	if _, err := src.Modify(ctx, entroq.InsertingInto(outbox, entroq.WithRawValue(json.RawMessage(want)))); err != nil {
		t.Fatalf("insert source task: %v", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return Run(gctx, src,
			WithDest(dst),
			WithInbox(inbox),
			WithQueues(outbox),
			WithSource("A"),
		)
	})

	// Source drains only after delivery + source delete.
	if err := src.WaitQueuesEmpty(gctx, entroq.MatchExact(outbox)); err != nil {
		t.Fatalf("wait source drain: %v", err)
	}

	// The inbox task and the tombstone are inserted in one atomic Modify, so the
	// delivered task's presence proves the tombstone was created. Claim it first
	// to establish that, which makes the subsequent "graveyard empty" check mean
	// "created then cleaned" rather than "never created".
	task, err := dst.Claim(gctx, entroq.From(inbox), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("claim delivered task: %v", err)
	}
	if got := string(task.Value); got != want {
		t.Errorf("delivered value = %s, want %s", got, want)
	}

	// Given the tombstone was created (above), an empty graveyard means the
	// happy path eager-cleaned it.
	if err := dst.WaitQueuesEmpty(gctx, entroq.MatchExact(defaultGraveyard(inbox))); err != nil {
		t.Fatalf("wait tombstone cleanup: %v", err)
	}

	cancel()
	if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
		t.Fatalf("worker: %v", err)
	}
}

// TestPullDedupOnRedelivery is the load-bearing test. It models a prior attempt
// that delivered and then crashed before deleting the source -- and whose inbox
// task has since been consumed -- so only the tombstone remains. A re-delivery
// must collide on that tombstone and produce no inbox task, with dedup resting
// entirely on the tombstone (not on the long-gone inbox task).
func TestPullDedupOnRedelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := memClient(ctx, t)
	dst := memClient(ctx, t)

	const (
		outbox = "/out"
		inbox  = "/in"
		source = "A"
	)
	want := json.RawMessage(`"hello"`)

	resp, err := src.Modify(ctx, entroq.InsertingInto(outbox, entroq.WithRawValue(want)))
	if err != nil {
		t.Fatalf("insert source task: %v", err)
	}
	srcTask := resp.InsertedTasks[0]

	// Seed only the tombstone (the prior delivery's inbox task is gone). Keep the
	// seeded version so we can prove afterward that the worker collided on this
	// exact tombstone rather than inserting a fresh one.
	tombID := (&Worker{source: source}).transferID(srcTask)
	seedResp, err := dst.Modify(ctx, entroq.InsertingInto(defaultGraveyard(inbox),
		entroq.WithID(tombID), entroq.WithArrivalTimeIn(time.Hour)))
	if err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	seeded := seedResp.InsertedTasks[0]

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return Run(gctx, src, WithDest(dst), WithInbox(inbox), WithQueues(outbox), WithSource(source))
	})

	if err := src.WaitQueuesEmpty(gctx, entroq.MatchExact(outbox)); err != nil {
		t.Fatalf("wait source drain: %v", err)
	}

	// The collision was actually exercised: the seeded tombstone is still the only
	// one present and untouched. A non-collision (e.g. a transferID change) would
	// have inserted a second tombstone under a different id.
	tombs, err := dst.Tasks(ctx, defaultGraveyard(inbox))
	if err != nil {
		t.Fatalf("list tombstones: %v", err)
	}
	if len(tombs) != 1 {
		t.Fatalf("tombstone count = %d, want 1 (re-delivery must collide, not add a tombstone)", len(tombs))
	}
	if tombs[0].ID != tombID || tombs[0].Version != seeded.Version {
		t.Errorf("tombstone = %v, want the seeded %v unchanged", tombs[0].IDVersion(), seeded.IDVersion())
	}

	// And no duplicate inbox task was produced.
	inboxTasks, err := dst.Tasks(ctx, inbox)
	if err != nil {
		t.Fatalf("list inbox: %v", err)
	}
	if len(inboxTasks) != 0 {
		t.Errorf("inbox task count = %d, want 0 (re-delivery must not duplicate)", len(inboxTasks))
	}

	cancel()
	if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
		t.Fatalf("worker: %v", err)
	}
}

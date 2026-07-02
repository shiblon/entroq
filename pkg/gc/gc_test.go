package gc_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/gc"
)

func newTestEQ(ctx context.Context, t *testing.T) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open eqmem: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
}

// TestRun verifies that a single scan drains queues whose gc= activation time
// has passed while leaving not-yet-due, undirected, malformed, and legacy exp=
// queues untouched.
func TestRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq := newTestEQ(ctx, t)

	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()

	cases := []struct {
		name      string
		queue     string
		wantEmpty bool
	}{
		{"canonical gc expired", fmt.Sprintf("/svc/gc=%d", past), true},
		{"gc always on", "/svc/gc=0", true},
		{"gc not yet due", fmt.Sprintf("/svc/gc=%d/leaf", future), false},
		{"exp is ignored", fmt.Sprintf("/svc/response/exp=%d/aaaa", past), false},
		{"no gc directive", "/svc/plain", false},
		{"malformed value is left alone", "/svc/gc=notatime", false},
	}

	for _, tc := range cases {
		if _, err := eq.Modify(ctx, entroq.InsertingInto(tc.queue, entroq.WithRawValue([]byte("{}")))); err != nil {
			t.Fatalf("insert into %q: %v", tc.queue, err)
		}
	}

	if err := gc.Run(ctx, eq); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sizes, err := eq.Queues(ctx)
	if err != nil {
		t.Fatalf("Queues: %v", err)
	}
	for _, tc := range cases {
		got := sizes[tc.queue]
		switch {
		case tc.wantEmpty && got != 0:
			t.Errorf("%s: queue %q should have been collected, still has %d task(s)", tc.name, tc.queue, got)
		case !tc.wantEmpty && got != 1:
			t.Errorf("%s: queue %q should have been left alone, has %d task(s) (want 1)", tc.name, tc.queue, got)
		}
	}
}

// TestRunBatches verifies that a queue holding more tasks than one batch is
// fully drained across multiple batches (with a pause between them).
func TestRunBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq := newTestEQ(ctx, t)

	past := time.Now().Add(-time.Hour).Unix()
	q := fmt.Sprintf("/svc/gc=%d", past)

	const n = 5
	for i := 0; i < n; i++ {
		if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithRawValue([]byte("{}")))); err != nil {
			t.Fatalf("insert %d into %q: %v", i, q, err)
		}
	}

	// MaxSize below the task count forces multiple batches; a tiny pause keeps
	// the test fast while still exercising the between-batch rest path.
	if err := gc.Run(ctx, eq, gc.WithMaxSize(2), gc.WithBatchPause(time.Millisecond)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sizes, err := eq.Queues(ctx)
	if err != nil {
		t.Fatalf("Queues: %v", err)
	}
	if got := sizes[q]; got != 0 {
		t.Errorf("queue %q should be fully drained across batches, has %d task(s)", q, got)
	}
}

// TestRunRespectsMatch verifies that WithMatch scopes the scan: an expired
// queue outside the match is not collected.
func TestRunRespectsMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq := newTestEQ(ctx, t)

	past := time.Now().Add(-time.Hour).Unix()
	inScope := fmt.Sprintf("/svc/gc=%d", past)
	outOfScope := fmt.Sprintf("/other/gc=%d", past)

	for _, q := range []string{inScope, outOfScope} {
		if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithRawValue([]byte("{}")))); err != nil {
			t.Fatalf("insert into %q: %v", q, err)
		}
	}

	if err := gc.Run(ctx, eq, gc.WithMatch(entroq.MatchPrefix("/svc"))); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sizes, err := eq.Queues(ctx)
	if err != nil {
		t.Fatalf("Queues: %v", err)
	}
	if got := sizes[inScope]; got != 0 {
		t.Errorf("in-scope queue %q should have been collected, still has %d task(s)", inScope, got)
	}
	if got := sizes[outOfScope]; got != 1 {
		t.Errorf("out-of-scope queue %q should have been left alone, has %d task(s) (want 1)", outOfScope, got)
	}
}

package async_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestRunGC verifies that a single GC pass drains queues whose gc=/exp=
// activation time has passed while leaving not-yet-due, undirected, and
// malformed queues untouched.
func TestRunGC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, stop := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stop()

	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()

	cases := []struct {
		name      string
		queue     string
		wantEmpty bool
	}{
		{"legacy exp expired", fmt.Sprintf("/svc/response/exp=%d/aaaa", past), true},
		{"canonical gc expired", fmt.Sprintf("/svc/gc=%d", past), true},
		{"gc always on", "/svc/gc=0", true},
		{"exp not yet due", fmt.Sprintf("/svc/response/exp=%d/bbbb", future), false},
		{"no gc directive", "/svc/plain", false},
		{"malformed value is left alone", "/svc/gc=notatime", false},
	}

	for _, tc := range cases {
		if _, err := eq.Modify(ctx, entroq.InsertingInto(tc.queue, entroq.WithRawValue([]byte("{}")))); err != nil {
			t.Fatalf("insert into %q: %v", tc.queue, err)
		}
	}

	if err := async.RunGC(ctx, eq); err != nil {
		t.Fatalf("RunGC: %v", err)
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

// TestRunGCRespectsMatch verifies that WithGCMatch scopes the scan: an expired
// queue outside the match is not collected.
func TestRunGCRespectsMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, stop := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stop()

	past := time.Now().Add(-time.Hour).Unix()
	inScope := fmt.Sprintf("/svc/gc=%d", past)
	outOfScope := fmt.Sprintf("/other/gc=%d", past)

	for _, q := range []string{inScope, outOfScope} {
		if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithRawValue([]byte("{}")))); err != nil {
			t.Fatalf("insert into %q: %v", q, err)
		}
	}

	if err := async.RunGC(ctx, eq, async.WithGCMatch(entroq.MatchPrefix("/svc"))); err != nil {
		t.Fatalf("RunGC: %v", err)
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

package eqtest

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
)
func QueueMatch(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue1 := path.Join(qPrefix, "queue-1")
	queue2 := path.Join(qPrefix, "queue-2")
	queue3 := path.Join(qPrefix, "queue-3")
	quirkyQueue := path.Join(qPrefix, "quirky=queue")

	wantQueues := map[string]int{
		queue1:      1,
		queue2:      2,
		queue3:      3,
		quirkyQueue: 1,
	}

	// Add tasks so that queues have a certain number of things in them, as above.
	var toInsert []entroq.ModifyArg
	for q, n := range wantQueues {
		for i := 0; i < n; i++ {
			toInsert = append(toInsert, entroq.InsertingInto(q))
		}
	}
	inserted, _, err := client.Modify(ctx, toInsert...)
	if err != nil {
		t.Fatalf("in QueueMatch - inserting empty tasks: %v", err)
	}

	// Check that we got everything inserted.
	if want, got := len(inserted), len(toInsert); want != got {
		t.Fatalf("in QueueMatch - want %d inserted, got %d", want, got)
	}

	// Check that we can get exact numbers for all of the above using MatchExact.
	for q, n := range wantQueues {
		qs, err := client.Queues(ctx, entroq.MatchExact(q))
		if err != nil {
			t.Fatalf("QueueMatch single - getting queue: %v", err)
		}
		if len(qs) != 1 {
			t.Errorf("QueueMatch single - expected 1 entry, got %d", len(qs))
		}
		if want, got := n, qs[q]; want != got {
			t.Errorf("QueueMatch single - expected %d values in queue %q, got %d", want, q, got)
		}
	}

	// Check that passing multiple exact matches works properly.
	multiExactCases := []struct {
		q1 string
		q2 string
	}{
		{queue1, queue2},
		{queue1, queue3},
		{quirkyQueue, queue2},
		{"bogus", queue3},
	}

	for _, c := range multiExactCases {
		qs, err := client.Queues(ctx, entroq.MatchExact(c.q1), entroq.MatchExact(c.q2))
		if err != nil {
			t.Fatalf("QueueMatch multi - getting multiple queues: %v", err)
		}
		if len(qs) > 2 {
			t.Errorf("QueueMatch multi - expected no more than 2 entries, got %d", len(qs))
		}
		want1, want2 := wantQueues[c.q1], wantQueues[c.q2]
		if got1, got2 := qs[c.q1], qs[c.q2]; want1 != got1 || want2 != got2 {
			t.Errorf("QueueMatch multi - wanted %q:%d, %q:%d, got %q:%d, %q:%d", c.q1, want1, c.q2, want2, c.q1, got1, c.q2, got2)
		}
	}

	// Check prefix matching.
	prefixCases := []struct {
		prefix string
		qn     int
		n      int
	}{
		{path.Join(qPrefix, "queue-"), 3, 6},
		{path.Join(qPrefix, "qu"), 4, 7},
		{path.Join(qPrefix, "qui"), 1, 1},
	}

	for _, c := range prefixCases {
		qs, err := client.Queues(ctx, entroq.MatchPrefix(c.prefix))
		if err != nil {
			t.Fatalf("QueueMatch prefix - queues error: %v", err)
		}
		if want, got := c.qn, len(qs); want != got {
			t.Errorf("QueueMatch prefix - want %d queues, got %d", want, got)
		}
		tot := 0
		for _, n := range qs {
			tot += n
		}
		if want, got := c.n, tot; want != got {
			t.Errorf("QueueMatch prefix - want %d total items, got %d", want, got)
		}
	}
}

// QueueStats checks that queue stats basically work.
func QueueStats(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	nothingClaimedQueue := path.Join(qPrefix, "queue-1")
	partiallyClaimedQueue := path.Join(qPrefix, "queue-2")

	if _, _, err := client.Modify(ctx,
		entroq.InsertingInto(nothingClaimedQueue),
		entroq.InsertingInto(nothingClaimedQueue),
		entroq.InsertingInto(partiallyClaimedQueue),
		entroq.InsertingInto(partiallyClaimedQueue),
		entroq.InsertingInto(partiallyClaimedQueue),
	); err != nil {
		t.Fatalf("Insert into queues: %v", err)
	}

	// Now claim something.
	if _, err := client.Claim(ctx, entroq.From(partiallyClaimedQueue)); err != nil {
		t.Fatalf("Couldn't claim: %v", err)
	}

	got, err := client.QueueStats(ctx, entroq.MatchPrefix(qPrefix))
	if err != nil {
		t.Fatalf("Queue stats error: %v", err)
	}

	want := map[string]*entroq.QueueStat{
		nothingClaimedQueue: {
			Name:      nothingClaimedQueue,
			Size:      2,
			Claimed:   0,
			Available: 2,
			MaxClaims: 0,
		},
		partiallyClaimedQueue: {
			Name:      partiallyClaimedQueue,
			Size:      3,
			Claimed:   1,
			Available: 2,
			MaxClaims: 1,
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("QueueStats (-want +got):\n%v", diff)
	}
}

// QueueStatsLimit verifies that the Limit option restricts the number of queues
// returned by QueueStats. Regression test for a LIMIT/GROUP BY ordering bug.
func QueueStatsLimit(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		q := path.Join(qPrefix, fmt.Sprintf("queue-%d", i))
		if _, _, err := client.Modify(ctx, entroq.InsertingInto(q)); err != nil {
			t.Fatalf("Insert into %v: %v", q, err)
		}
	}

	got, err := client.QueueStats(ctx, entroq.MatchPrefix(qPrefix), entroq.LimitQueues(2))
	if err != nil {
		t.Fatalf("QueueStats with limit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("QueueStats with limit=2: got %d queues, want 2", len(got))
	}
}

type taskQueueVersionValue struct {
	Queue   string
	Version int32
	Value   json.RawMessage
}


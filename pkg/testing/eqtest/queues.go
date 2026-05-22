package eqtest

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"testing"
	"time"

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
	resp, err := client.Modify(ctx, toInsert...)
	if err != nil {
		t.Fatalf("in QueueMatch - inserting empty tasks: %v", err)
	}
	inserted := resp.InsertedTasks

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

	if _, err := client.Modify(ctx,
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
		if _, err := client.Modify(ctx, entroq.InsertingInto(q)); err != nil {
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

// QueueStatsAccuracy verifies Claimed and Future counts without checking MaxClaims,
// allowing this test to run on backends that always return MaxClaims=0.
func QueueStatsAccuracy(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	unclaimed := path.Join(qPrefix, "qs-acc-1")
	partial := path.Join(qPrefix, "qs-acc-2")

	if _, err := client.Modify(ctx,
		entroq.InsertingInto(unclaimed),
		entroq.InsertingInto(unclaimed),
		entroq.InsertingInto(partial),
		entroq.InsertingInto(partial),
		entroq.InsertingInto(partial),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := client.Claim(ctx, entroq.From(partial)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	got, err := client.QueueStats(ctx, entroq.MatchPrefix(qPrefix))
	if err != nil {
		t.Fatalf("QueueStats: %v", err)
	}

	for _, tc := range []struct {
		name      string
		wantSize  int
		wantClaim int
		wantAvail int
	}{
		{unclaimed, 2, 0, 2},
		{partial, 3, 1, 2},
	} {
		s, ok := got[tc.name]
		if !ok {
			t.Errorf("queue %q missing from stats", tc.name)
			continue
		}
		if s.Size != tc.wantSize {
			t.Errorf("queue %q: Size want %d got %d", tc.name, tc.wantSize, s.Size)
		}
		if s.Claimed != tc.wantClaim {
			t.Errorf("queue %q: Claimed want %d got %d", tc.name, tc.wantClaim, s.Claimed)
		}
		if s.Available != tc.wantAvail {
			t.Errorf("queue %q: Available want %d got %d", tc.name, tc.wantAvail, s.Available)
		}
		if future := s.Size - s.Claimed - s.Available; future < 0 {
			t.Errorf("queue %q: Future is negative (%d)", tc.name, future)
		}
	}
}

// NamespaceStats verifies doc namespace statistics: size, claimed count,
// prefix/exact filtering, and limit.
func NamespaceStats(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	ns1 := path.Join(qPrefix, "ns-stats-1")
	ns2 := path.Join(qPrefix, "ns-stats-2")
	ns3 := path.Join(qPrefix, "ns-other")
	const key = "k"

	// Insert 2 docs in ns1, 3 in ns2, 1 in ns3.
	if _, err := client.Modify(ctx,
		entroq.CreatingIn(ns1, entroq.WithKeys(key, "a")),
		entroq.CreatingIn(ns1, entroq.WithKeys(key, "b")),
		entroq.CreatingIn(ns2, entroq.WithKeys(key, "a")),
		entroq.CreatingIn(ns2, entroq.WithKeys(key, "b")),
		entroq.CreatingIn(ns2, entroq.WithKeys(key, "c")),
		entroq.CreatingIn(ns3, entroq.WithKeys(key, "a")),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Claim one doc in ns2.
	claimed, err := client.ClaimDocs(ctx, &entroq.DocClaim{
		Namespace: ns2,
		Claimant:  client.ClientID,
		Key:       key,
		Duration:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("claim docs: %v", err)
	}

	// All namespaces.
	got, err := client.NamespaceStats(ctx, entroq.MatchPrefix(qPrefix))
	if err != nil {
		t.Fatalf("NamespaceStats all: %v", err)
	}
	for _, tc := range []struct{ ns string; size, claimed int }{
		{ns1, 2, 0},
		{ns2, 3, 3}, // all docs under key "k" were claimed together
		{ns3, 1, 0},
	} {
		s, ok := got[tc.ns]
		if !ok {
			t.Errorf("namespace %q missing", tc.ns)
			continue
		}
		if s.Size != tc.size {
			t.Errorf("namespace %q: Size want %d got %d", tc.ns, tc.size, s.Size)
		}
		if s.Claimed != tc.claimed {
			t.Errorf("namespace %q: Claimed want %d got %d", tc.ns, tc.claimed, s.Claimed)
		}
	}

	// Release the claim: change each claimed doc with a past at.
	for _, d := range claimed {
		if _, err := client.Modify(ctx, d.Change(entroq.WithDocArrivalTimeBy(-time.Second))); err != nil {
			t.Fatalf("release doc: %v", err)
		}
	}

	// After release, ns2 claimed should be 0.
	got2, err := client.NamespaceStats(ctx, entroq.MatchExact(ns2))
	if err != nil {
		t.Fatalf("NamespaceStats after release: %v", err)
	}
	if s := got2[ns2]; s == nil || s.Claimed != 0 {
		t.Errorf("after release: ns2 Claimed want 0, got %v", got2[ns2])
	}

	// Prefix filter: only ns-stats-1 and ns-stats-2.
	gotPrefix, err := client.NamespaceStats(ctx, entroq.MatchPrefix(path.Join(qPrefix, "ns-stats-")))
	if err != nil {
		t.Fatalf("NamespaceStats prefix: %v", err)
	}
	if len(gotPrefix) != 2 {
		t.Errorf("prefix filter: want 2 namespaces, got %d", len(gotPrefix))
	}

	// Limit.
	gotLimit, err := client.NamespaceStats(ctx, entroq.MatchPrefix(qPrefix), entroq.WithLimit(2))
	if err != nil {
		t.Fatalf("NamespaceStats limit: %v", err)
	}
	if len(gotLimit) != 2 {
		t.Errorf("limit=2: want 2 namespaces, got %d", len(gotLimit))
	}
}

type taskQueueVersionValue struct {
	Queue   string
	Version int32
	Value   json.RawMessage
}


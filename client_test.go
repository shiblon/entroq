package entroq_test

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestAsMakesSiblingsNotNests covers the one way this design can silently
// invert. Each scoped client appends its own claimant to the arguments going
// down, so a client wrapping a client would leave the INNER claimant last, and
// last wins -- quietly, and only for writes. Every scoped client is one hop
// from the connection, so that cannot arise.
func TestAsMakesSiblingsNotNests(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener(), entroq.WithClaimantID("connection"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	a := eq.As("a")
	b := a.As("b")

	if got := b.ID(); got != "b" {
		t.Errorf("a client made from another holds as %q, want %q", got, "b")
	}
	if got := a.ID(); got != "a" {
		t.Errorf("making a sibling changed the original: %q", got)
	}
	if got := eq.ID(); got != "connection" {
		t.Errorf("making a client changed the connection's own claimant: %q", got)
	}

	// The proof that b is a sibling and not a wrapper: what it writes is held
	// by b. Were it nested, a's claimant would be appended last and win.
	const queue = "as_siblings"
	resp, err := b.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(time.Hour)))
	if err != nil {
		t.Fatalf("Insert as b: %v", err)
	}
	if got := resp.InsertedTasks[0].Claimant; got != "b" {
		t.Errorf("a task written by a client made from another is held by %q, want %q", got, "b")
	}
}

// TestScopedClientClaimantWinsOverTheCallers pins the ordering rule: the
// claimant is appended last, so it beats anything the caller passed.
//
// That is deliberate rather than incidental. The claimant is what holds the
// lease a modification depends on, so honoring a caller's own ModifyAs would
// commit under an identity that holds nothing.
func TestScopedClientClaimantWinsOverTheCallers(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener(), entroq.WithClaimantID("connection"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	const queue = "as_override"
	resp, err := eq.As("owner").Modify(ctx,
		entroq.ModifyAs("impostor"),
		entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(time.Hour)))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got := resp.InsertedTasks[0].Claimant; got != "owner" {
		t.Errorf("task held by %q, want the scoped client's %q: a caller's own claimant must not win", got, "owner")
	}
}

// TestDistinctClaimantsExcludeEachOther is why Client exists.
//
// Doc sets exclude by claimant. Two consumers sharing one claimant can claim
// each other's sets, and nothing reports it -- the exclusion simply stops
// excluding. Here the two consumers share a CONNECTION and not a claimant,
// which is the arrangement a process running several workers needs.
func TestDistinctClaimantsExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	const ns = "claimant_exclusion"
	one, two := eq.As("consumer-1"), eq.As("consumer-2")

	if _, err := one.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("shared", ""))); err != nil {
		t.Fatalf("Insert doc: %v", err)
	}

	sets, err := one.ClaimDocs(ctx, entroq.ClaimKey(ns, "shared"), entroq.ClaimingSetsFor(time.Hour))
	if err != nil {
		t.Fatalf("Claim as consumer-1: %v", err)
	}
	if got := sets[0].Claimant; got != "consumer-1" {
		t.Errorf("set held by %q, want %q", got, "consumer-1")
	}

	// The whole point: the second consumer is excluded, even though it speaks
	// through the same connection.
	if _, err := two.ClaimDocs(ctx, entroq.ClaimKey(ns, "shared"), entroq.ClaimingSetsFor(time.Hour)); !entroq.IsDependency(err) {
		t.Errorf("Claim of a held set by another consumer on the same connection: want a dependency error, got %v", err)
	}

	// And the connection's own claimant is a third consumer, excluded too.
	if _, err := eq.ClaimDocs(ctx, entroq.ClaimKey(ns, "shared"), entroq.ClaimingSetsFor(time.Hour)); !entroq.IsDependency(err) {
		t.Errorf("Claim of a held set by the connection itself: want a dependency error, got %v", err)
	}

	// The holder may of course renew what it holds.
	if _, err := one.UpdateArrival(ctx, entroq.ReadyIn(time.Hour).Docs(sets...)); err != nil {
		t.Errorf("Renewal by the holder: %v", err)
	}

	// The counterfactual, so this test is known to be measuring exclusion
	// rather than something that would pass anyway: two consumers that SHARE a
	// claimant do not exclude each other. This is the hazard, demonstrated --
	// the second claim succeeds and takes a set the first still believes it
	// holds, with nothing reported to either of them.
	const shared = "shared_claimant"
	if _, err := one.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys(shared, ""))); err != nil {
		t.Fatalf("Insert second doc: %v", err)
	}
	same1, same2 := eq.As("one-consumer"), eq.As("one-consumer")
	if _, err := same1.ClaimDocs(ctx, entroq.ClaimKey(ns, shared), entroq.ClaimingSetsFor(time.Hour)); err != nil {
		t.Fatalf("First claim under a shared claimant: %v", err)
	}
	if _, err := same2.ClaimDocs(ctx, entroq.ClaimKey(ns, shared), entroq.ClaimingSetsFor(time.Hour)); err != nil {
		t.Errorf("Second claim under a SHARED claimant: got %v, want it to succeed -- if this now fails, exclusion no longer keys on the claimant and this test proves less than it claims", err)
	}
}

// TestScopedClientReadsNeedNoClaimant covers the other half of the split: a
// read records no holder, so it answers the same for every consumer.
func TestScopedClientReadsNeedNoClaimant(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	const ns = "claimant_reads"
	if _, err := eq.As("writer").Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithKeys("config", ""), entroq.WithContent("v1"))); err != nil {
		t.Fatalf("Insert doc: %v", err)
	}

	// A different consumer reads it without claiming anything, which is the
	// case a worker body needs for a config doc.
	docs, err := eq.As("reader").Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil {
		t.Fatalf("Docs as reader: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if _, err := eq.As("reader").Time(ctx); err != nil {
		t.Errorf("Time as reader: %v", err)
	}
}

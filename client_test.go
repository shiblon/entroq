package entroq_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestScopedMakesSiblingsNotNests covers the one way this design can silently
// invert. Each scoped client appends its own claimant to the arguments going
// down, so a client wrapping a client would leave the INNER claimant last, and
// last wins -- quietly, and only for writes. Every scoped client is one hop
// from the connection, so that cannot arise.
func TestScopedMakesSiblingsNotNests(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener(), entroq.WithClaimantID("connection"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	a := eq.Scoped(entroq.DangerousClaimant("a"))
	b := a.Scoped(entroq.DangerousClaimant("b"))

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
	resp, err := eq.Scoped(entroq.DangerousClaimant("owner")).Modify(ctx,
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
	one, two := eq.Scoped(entroq.DangerousClaimant("consumer-1")), eq.Scoped(entroq.DangerousClaimant("consumer-2"))

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
	same1, same2 := eq.Scoped(entroq.DangerousClaimant("one-consumer")), eq.Scoped(entroq.DangerousClaimant("one-consumer"))
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
	if _, err := eq.Scoped(entroq.DangerousClaimant("writer")).Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithKeys("config", ""), entroq.WithContent("v1"))); err != nil {
		t.Fatalf("Insert doc: %v", err)
	}

	// A different consumer reads it without claiming anything, which is the
	// case a worker body needs for a config doc.
	docs, err := eq.Scoped(entroq.DangerousClaimant("reader")).Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil {
		t.Fatalf("Docs as reader: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if _, err := eq.Scoped(entroq.DangerousClaimant("reader")).Time(ctx); err != nil {
		t.Errorf("Time as reader: %v", err)
	}
}

// testJWT is an unsigned token carrying sub: clients only read the subject,
// and the server is what verifies a signature.
func testJWT(sub string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		enc.EncodeToString([]byte(`{"sub":"`+sub+`"}`)) + ".sig"
}

// TestSubjectFromJWTBeginsEveryClaimant checks that a client given a token's
// subject generates every claimant after it, its own and each scoped one, each
// fresh, which is what mesh authz requires of a claimant.
func TestSubjectFromJWTBeginsEveryClaimant(t *testing.T) {
	ctx := context.Background()
	const sub = "system:serviceaccount:entroq:worker"
	eq, err := entroq.New(ctx, eqmem.Opener(), entroq.WithSubjectFromJWT(testJWT(sub)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	seen := map[string]bool{}
	for _, c := range []entroq.Client{eq, eq.Scoped(), eq.Scoped(), eq.Scoped().Scoped()} {
		id := c.ID()
		if !strings.HasPrefix(id, sub+entroq.ClaimantSeparator) || len(id) <= len(sub)+1 {
			t.Errorf("claimant %q does not begin with %q and a fresh ID", id, sub+entroq.ClaimantSeparator)
		}
		if seen[id] {
			t.Errorf("claimant %q generated twice", id)
		}
		seen[id] = true
	}

	// The override is taken as given.
	if got := eq.Scoped(entroq.DangerousClaimant("given")).ID(); got != "given" {
		t.Errorf("DangerousClaimant made %q, want it as given", got)
	}

	// Without a subject a claimant is a fresh ID alone.
	plain, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer plain.Close()
	if id := plain.Scoped().ID(); strings.Contains(id, entroq.ClaimantSeparator) || id == plain.ID() {
		t.Errorf("subjectless scoped claimant %q, want a fresh ID unlike %q", id, plain.ID())
	}
}

// TestSubjectFromJWTRefusesWhatItCannotUse checks that New reports a token it
// cannot read, or a subject too long to leave room for a generated ID, rather
// than making claimants every request would be refused for.
func TestSubjectFromJWTRefusesWhatItCannotUse(t *testing.T) {
	ctx := context.Background()
	for name, tok := range map[string]string{
		"not a JWT":        "opaque-token",
		"no sub":           testJWT(""),
		"subject too long": testJWT(strings.Repeat("s", entroq.MaxClaimantBytes)),
	} {
		t.Run(name, func(t *testing.T) {
			eq, err := entroq.New(ctx, eqmem.Opener(), entroq.WithSubjectFromJWT(tok))
			if err == nil {
				eq.Close()
				t.Fatal("New accepted it")
			}
			if !entroq.IsInvalidArgument(err) {
				t.Errorf("New: %v, want an invalid argument", err)
			}
		})
	}
}

// TestClaimedBySelfMeansTheAskingClient: a scoped client's ClaimedBySelf is
// its own claimant, the one a worker Run holds its tasks as, and not the
// connection's, which holds none of them.
func TestClaimedBySelfMeansTheAskingClient(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	const queue = "claimed_by_self"
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	run := eq.Scoped()
	held, err := run.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute))
	if err != nil || held == nil {
		t.Fatalf("TryClaim: %v, %v", held, err)
	}

	mine, err := run.Tasks(ctx, queue, entroq.ClaimedBySelf())
	if err != nil {
		t.Fatalf("Tasks as the run: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != held.ID {
		t.Errorf("the run's ClaimedBySelf found %v, want the task it holds, %s", mine, held.ID)
	}
	theirs, err := eq.Tasks(ctx, queue, entroq.ClaimedBySelf())
	if err != nil {
		t.Fatalf("Tasks as the connection: %v", err)
	}
	if len(theirs) != 0 {
		t.Errorf("the connection's ClaimedBySelf found %v, a task the run holds", theirs)
	}
}

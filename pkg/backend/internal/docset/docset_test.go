package docset

import (
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// store is a fixed set of members and locks for Evaluate.
type store struct {
	members map[string]*entroq.Doc
	locks   map[Set]Lock
}

func (s store) member(ns, id string) *entroq.Doc { return s.members[entroq.DocKey(ns, id)] }
func (s store) lock(g Set) Lock {
	if l, ok := s.locks[g]; ok {
		return l
	}
	return Absent
}

func (s store) evaluate(claimant string, args ...entroq.ModifyArg) Plan {
	return Evaluate(entroq.NewModification(claimant, args...), now, s.member, s.lock)
}

// newStore holds one stored set, ns/k, with members a and b at version 5.
func newStore(l Lock) store {
	l.Stored, l.Version, l.NumDocs = true, 5, 2
	return store{
		members: map[string]*entroq.Doc{
			entroq.DocKey("ns", "a"): {Namespace: "ns", ID: "a", Key: "k"},
			entroq.DocKey("ns", "b"): {Namespace: "ns", ID: "b", Key: "k"},
		},
		locks: map[Set]Lock{{Namespace: "ns", Key: "k"}: l},
	}
}

var set = Set{Namespace: "ns", Key: "k"}

func doc(id string, version int32) *entroq.Doc {
	return &entroq.Doc{Namespace: "ns", ID: id, Key: "k", Version: version}
}

func TestEvaluateWriteMovesVersionOnceAndReleases(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Minute)})
	p := s.evaluate("me",
		doc("a", 5).Change(entroq.WithContent("x")),
		doc("b", 5).Delete(),
		entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")),
	)
	if p.Err != nil {
		t.Fatalf("Evaluate: %v", p.Err)
	}
	got, ok := p.Locks[set]
	if !ok || len(p.Locks) != 1 {
		t.Fatalf("Locks: want one lock for %v, got %v", set, p.Locks)
	}
	if got.Version != 6 || got.Claimant != "" || !got.At.Equal(now) {
		t.Errorf("Holder write: want released at version 6, got %+v", got)
	}
}

func TestEvaluateFutureArrivalKeepsGroupHeld(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Minute)})
	// Arrivals are durations from the backend's now; later is the instant the
	// longer of them resolves to, which the lock below must carry.
	later := now.Add(time.Hour)
	p := s.evaluate("me",
		doc("a", 5).Change(entroq.WithDocArrivalTimeBy(time.Minute)),
		doc("b", 5).Change(entroq.WithDocArrivalTimeBy(time.Hour)),
	)
	if p.Err != nil {
		t.Fatalf("Evaluate: %v", p.Err)
	}
	got := p.Locks[set]
	if got.Version != 6 || got.Claimant != "me" || !got.At.Equal(later) {
		t.Errorf("Renewing write: want held by me until the latest arrival at version 6, got %+v", got)
	}
}

func TestEvaluateRejectsWritesToGroupHeldByOther(t *testing.T) {
	s := newStore(Lock{Claimant: "holder", At: now.Add(time.Minute)})
	cases := map[string]entroq.ModifyArg{
		"change": doc("a", 5).Change(entroq.WithContent("x")),
		"delete": doc("a", 5).Delete(),
		"insert": entroq.PuttingDocInto("ns", entroq.WithIDKeys("c", "k", "")),
	}
	for name, arg := range cases {
		p := s.evaluate("intruder", arg)
		if p.Err == nil || !p.Err.HasClaimedDocs() || p.Err.HasMissingDocs() {
			t.Errorf("Intruder %s: want only a claim failure, got %v", name, p.Err)
		}
		if len(p.Locks) != 0 {
			t.Errorf("Intruder %s: want no lock changes, got %v", name, p.Locks)
		}
	}

	// The holder, or a caller acting as the holder, may write.
	if p := s.evaluate("holder", cases["insert"]); p.Err != nil {
		t.Errorf("Holder insert: %v", p.Err)
	}
}

func TestEvaluateDependReadsWithoutClaimCheck(t *testing.T) {
	s := newStore(Lock{Claimant: "holder", At: now.Add(time.Minute)})
	p := s.evaluate("intruder", doc("a", 5).Depend())
	if p.Err != nil {
		t.Errorf("Depend on a held set: %v", p.Err)
	}
	if len(p.Locks) != 0 {
		t.Errorf("Depend: want no lock changes, got %v", p.Locks)
	}
	if p := s.evaluate("intruder", doc("a", 4).Depend()); p.Err == nil || len(p.Err.DocDepends) != 1 {
		t.Errorf("Depend at a stale version: want a depend failure, got %v", p.Err)
	}
}

func TestEvaluateChecksGroupVersion(t *testing.T) {
	s := newStore(Lock{})
	// A member named at any version but its set's is stale, even if it has
	// not itself changed.
	p := s.evaluate("me", doc("a", 4).Change(entroq.WithContent("x")), doc("b", 3).Delete())
	if p.Err == nil || len(p.Err.DocChanges) != 1 || len(p.Err.DocDeletes) != 1 {
		t.Errorf("Stale versions: want one change and one delete failure, got %v", p.Err)
	}
}

func TestEvaluateUsesStoredKey(t *testing.T) {
	s := newStore(Lock{Claimant: "holder", At: now.Add(time.Minute)})
	// The request names another key, but the stored member belongs to ns/k,
	// which someone else holds.
	chg := &entroq.Doc{Namespace: "ns", ID: "a", Key: "elsewhere", Version: 5}
	if p := s.evaluate("intruder", chg.Change(entroq.WithContent("x"))); p.Err == nil || !p.Err.HasClaimedDocs() {
		t.Errorf("Change naming a different key: want a claim failure from the stored set, got %v", p.Err)
	}
}

func TestEvaluateInsertIntoNewGroup(t *testing.T) {
	s := store{members: map[string]*entroq.Doc{}, locks: map[Set]Lock{}}
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("fresh", "")))
	if p.Err != nil {
		t.Fatalf("Insert: %v", p.Err)
	}
	if got := p.Locks[Set{Namespace: "ns", Key: "fresh"}]; got.Version != 0 || got.Claimant != "" {
		t.Errorf("New set: want version 0, unheld, got %+v", got)
	}
}

func TestEvaluateInsertCollision(t *testing.T) {
	s := newStore(Lock{})
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithIDKeys("a", "k", "")))
	if p.Err == nil || len(p.Err.DocInserts) != 1 {
		t.Errorf("Insert of an existing ID: want a collision, got %v", p.Err)
	}
}

func TestEvaluateExpiredClaimProtectsNothing(t *testing.T) {
	s := newStore(Lock{Claimant: "holder", At: now.Add(-time.Second)})
	if p := s.evaluate("other", doc("a", 5).Delete()); p.Err != nil {
		t.Errorf("Delete after the claim expired: %v", p.Err)
	}
}

func TestClaimNewGroupStartsAtOne(t *testing.T) {
	// Claiming a set nothing has stored claims the empty set that was in
	// effect already there at version 0, so it moves the version as any
	// other write does. Only an insert creates a set, at version 0; see
	// TestEvaluateInsertIntoNewGroup.
	if l, ok := Claim(Absent, "me", now, now.Add(time.Minute)); !ok || l.Version != 1 || !l.Stored {
		t.Errorf("First claim of a new set: want version 1, stored, got %+v, %v", l, ok)
	}
}

func TestClaim(t *testing.T) {
	l, ok := Claim(Lock{Stored: true, Version: 2}, "me", now, now.Add(time.Minute))
	if !ok || l.Version != 3 || l.Claimant != "me" || !l.At.Equal(now.Add(time.Minute)) {
		t.Fatalf("Claim of an unheld set: got %+v, %v", l, ok)
	}
	if again, ok := Claim(l, "me", now, now.Add(time.Hour)); !ok || again.Version != 4 || !again.At.Equal(now.Add(time.Hour)) {
		t.Errorf("Holder claiming again: want version 4 and the new expiry, got %+v, %v", again, ok)
	}
	if _, ok := Claim(l, "other", now, now.Add(time.Minute)); ok {
		t.Error("Claim of a set held by someone else succeeded")
	}
}

func TestEvaluateInsertIntoUnheldGroupMovesVersion(t *testing.T) {
	s := newStore(Lock{})
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")))
	if p.Err != nil {
		t.Fatalf("Insert: %v", p.Err)
	}
	if got := p.Locks[set]; got.Version != 6 || got.Claimant != "" {
		t.Errorf("Insert into an unheld set: want version 6, unheld, got %+v", got)
	}
	// A change in the same modification still moves the version, once.
	p = s.evaluate("me",
		entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")),
		doc("a", 5).Change(entroq.WithContent("x")),
	)
	if got := p.Locks[set]; p.Err != nil || got.Version != 6 {
		t.Errorf("Insert with a change: want version 6, got %+v, %v", got, p.Err)
	}
}

func TestEvaluateHolderInsertReleases(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Minute)})
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")))
	if got := p.Locks[set]; p.Err != nil || got.Version != 6 || got.Claimant != "" {
		t.Errorf("Holder insert: want released at version 6, got %+v, %v", got, p.Err)
	}
}

func TestEvaluateInsertWithArrivalClaims(t *testing.T) {
	s := newStore(Lock{})
	at := now.Add(time.Minute)
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c"), entroq.WithDocArrivalTimeBy(time.Minute)))
	if got := p.Locks[set]; p.Err != nil || got.Version != 6 || got.Claimant != "me" || !got.At.Equal(at) {
		t.Errorf("Insert with a future arrival: want held by me at version 6, got %+v, %v", got, p.Err)
	}
}

func TestEvaluateDeleteMovesVersion(t *testing.T) {
	s := newStore(Lock{})
	if got := s.evaluate("me", doc("a", 5).Delete()).Locks[set]; got.Version != 6 {
		t.Errorf("Delete from an unheld set: want version 6, got %+v", got)
	}
}

func TestExclusive(t *testing.T) {
	s := newStore(Lock{})
	mod := entroq.NewModification("me",
		entroq.PuttingDocInto("ns", entroq.WithKeys("appended", "")),
		entroq.PuttingDocInto("ns", entroq.WithKeys("claimed", ""), entroq.WithDocArrivalTimeBy(time.Minute)),
		doc("a", 5).Depend(),
	)
	got := Exclusive(mod, s.member)
	if len(got) != 2 || !got[Set{Namespace: "ns", Key: "appended"}] || !got[Set{Namespace: "ns", Key: "claimed"}] || got[set] {
		t.Errorf("Inserts and depends: want every insert's set exclusive and the depended-on one shared, got %v", got)
	}
	mod = entroq.NewModification("me", doc("a", 5).Change(entroq.WithContent("x")))
	if got := Exclusive(mod, s.member); !got[set] {
		t.Errorf("Change: want its set exclusive, got %v", got)
	}
}

func TestEvaluateCountsDocs(t *testing.T) {
	s := newStore(Lock{})
	p := s.evaluate("me",
		entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")),
		entroq.PuttingDocInto("ns", entroq.WithKeys("k", "d")),
		doc("a", 5).Delete(),
		doc("b", 5).Change(entroq.WithContent("x")),
	)
	if p.Err != nil {
		t.Fatalf("Evaluate: %v", p.Err)
	}
	if got := p.Locks[set].NumDocs; got != 3 {
		t.Errorf("Two inserts, a delete, and a change in a set of 2: want 3 docs, got %d", got)
	}
	if l, ok := Claim(s.lock(set), "me", now, now.Add(time.Minute)); !ok || l.NumDocs != 2 {
		t.Errorf("Claim: want the count kept at 2, got %+v, %v", l, ok)
	}
}

func TestEvaluateDocArrives(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Second)})
	renew := &entroq.DocSet{Namespace: "ns", Key: "k", Version: 5}
	// ReadyIn, not ReadyAt: an entry built from an instant is converted
	// against the real process clock when the modification is built, so a
	// fixed fake instant would resolve to a long-past duration and release the
	// set. A duration is the same here as it is on the wire.
	p := s.evaluate("me", entroq.Arriving(entroq.ReadyIn(time.Minute).Docs(renew)))
	if p.Err != nil {
		t.Fatalf("Evaluate: %v", p.Err)
	}
	l := p.Locks[set]
	if l.Version != 6 || l.Claimant != "me" || !l.At.Equal(now.Add(time.Minute)) || l.NumDocs != 2 {
		t.Errorf("Renewed set: want version 6 held by me for a minute with 2 docs, got %+v", l)
	}
	if len(p.Arrived) != 1 || p.Arrived[0].Version != 6 || p.Arrived[0].Docs != nil {
		t.Errorf("Arrived: want the set at its new lock without docs, got %+v", p.Arrived)
	}
	if p := s.evaluate("me", entroq.Arriving(entroq.ReadyNow().Docs(renew))); p.Locks[set].Claimant != "" {
		t.Errorf("Released set: want it unheld, got %+v", p.Locks[set])
	}
	stale := &entroq.DocSet{Namespace: "ns", Key: "k", Version: 4}
	if p := s.evaluate("me", entroq.Arriving(entroq.ReadyNow().Docs(stale))); p.Err == nil || len(p.Err.DocArrives) != 1 {
		t.Errorf("Stale set: want a set change failure, got %v", p.Err)
	}
	if p := s.evaluate("other", entroq.Arriving(entroq.ReadyNow().Docs(renew))); p.Err == nil || len(p.Err.DocClaims) != 1 || !p.Err.DocClaims[0].IsSetRef() {
		t.Errorf("Set held by someone else: want a set claim failure, got %v", p.Err)
	}
	// Version 0 is what an absent set reports, and a real stored set can hold
	// it too, so the version alone cannot carry this: an arrival naming a set
	// nothing has stored has to fail on the set not being there.
	absent := &entroq.DocSet{Namespace: "ns", Key: "none", Version: 0}
	if p := s.evaluate("me", entroq.Arriving(entroq.ReadyAt(now.Add(time.Minute)).Docs(absent))); p.Err == nil || len(p.Err.DocArrives) != 1 {
		t.Errorf("Absent set named at version 0: want an arrival failure, got %v", p.Err)
	}
}

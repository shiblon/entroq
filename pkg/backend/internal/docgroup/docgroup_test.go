package docgroup

import (
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// store is a fixed set of members and locks for Evaluate.
type store struct {
	members map[string]*entroq.Doc
	locks   map[Group]Lock
}

func (s store) member(ns, id string) *entroq.Doc { return s.members[entroq.DocKey(ns, id)] }
func (s store) lock(g Group) Lock {
	if l, ok := s.locks[g]; ok {
		return l
	}
	return Absent
}

func (s store) evaluate(claimant string, args ...entroq.ModifyArg) Plan {
	return Evaluate(entroq.NewModification(claimant, args...), now, s.member, s.lock)
}

// newStore holds one group, ns/k, with members a and b at version 5.
func newStore(l Lock) store {
	l.Version = 5
	return store{
		members: map[string]*entroq.Doc{
			entroq.DocKey("ns", "a"): {Namespace: "ns", ID: "a", Key: "k"},
			entroq.DocKey("ns", "b"): {Namespace: "ns", ID: "b", Key: "k"},
		},
		locks: map[Group]Lock{{Namespace: "ns", Key: "k"}: l},
	}
}

var group = Group{Namespace: "ns", Key: "k"}

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
	got, ok := p.Locks[group]
	if !ok || len(p.Locks) != 1 {
		t.Fatalf("Locks: want one lock for %v, got %v", group, p.Locks)
	}
	if got.Version != 6 || got.Claimant != "" || !got.At.Equal(now) {
		t.Errorf("Holder write: want released at version 6, got %+v", got)
	}
}

func TestEvaluateFutureArrivalKeepsGroupHeld(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Minute)})
	later := now.Add(time.Hour)
	p := s.evaluate("me",
		doc("a", 5).Change(entroq.WithDocArrivalTime(now.Add(time.Minute))),
		doc("b", 5).Change(entroq.WithDocArrivalTime(later)),
	)
	if p.Err != nil {
		t.Fatalf("Evaluate: %v", p.Err)
	}
	got := p.Locks[group]
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
		t.Errorf("Depend on a held group: %v", p.Err)
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
	// A member named at any version but its group's is stale, even if it has
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
		t.Errorf("Change naming a different key: want a claim failure from the stored group, got %v", p.Err)
	}
}

func TestEvaluateInsertIntoNewGroup(t *testing.T) {
	s := store{members: map[string]*entroq.Doc{}, locks: map[Group]Lock{}}
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("fresh", "")))
	if p.Err != nil {
		t.Fatalf("Insert: %v", p.Err)
	}
	if got := p.Locks[Group{Namespace: "ns", Key: "fresh"}]; got.Version != 0 || got.Claimant != "" {
		t.Errorf("New group: want version 0, unheld, got %+v", got)
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

func TestClaimNewGroupStartsAtZero(t *testing.T) {
	if l, ok := Claim(Absent, "me", now, time.Minute); !ok || l.Version != 0 {
		t.Errorf("First claim of a new group: want version 0, got %+v, %v", l, ok)
	}
}

func TestClaim(t *testing.T) {
	l, ok := Claim(Lock{Version: 2}, "me", now, time.Minute)
	if !ok || l.Version != 3 || l.Claimant != "me" || !l.At.Equal(now.Add(time.Minute)) {
		t.Fatalf("Claim of an unheld group: got %+v, %v", l, ok)
	}
	if again, ok := Claim(l, "me", now, time.Hour); !ok || again.Version != 4 || !again.At.Equal(now.Add(time.Hour)) {
		t.Errorf("Holder claiming again: want version 4 and the new expiry, got %+v, %v", again, ok)
	}
	if _, ok := Claim(l, "other", now, time.Minute); ok {
		t.Error("Claim of a group held by someone else succeeded")
	}
}

func TestEvaluateInsertIntoUnheldGroupLeavesLock(t *testing.T) {
	s := newStore(Lock{})
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")))
	if p.Err != nil {
		t.Fatalf("Insert: %v", p.Err)
	}
	if len(p.Locks) != 0 {
		t.Errorf("Insert into an unheld group: want no lock change, got %v", p.Locks)
	}
	// A change in the same modification still moves the version, once.
	p = s.evaluate("me",
		entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")),
		doc("a", 5).Change(entroq.WithContent("x")),
	)
	if got := p.Locks[group]; p.Err != nil || got.Version != 6 {
		t.Errorf("Insert with a change: want version 6, got %+v, %v", got, p.Err)
	}
}

func TestEvaluateHolderInsertReleases(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Minute)})
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c")))
	if got := p.Locks[group]; p.Err != nil || got.Version != 6 || got.Claimant != "" {
		t.Errorf("Holder insert: want released at version 6, got %+v, %v", got, p.Err)
	}
}

func TestEvaluateInsertWithArrivalClaims(t *testing.T) {
	s := newStore(Lock{})
	at := now.Add(time.Minute)
	p := s.evaluate("me", entroq.PuttingDocInto("ns", entroq.WithKeys("k", "c"), entroq.WithDocArrivalTime(at)))
	if got := p.Locks[group]; p.Err != nil || got.Version != 6 || got.Claimant != "me" || !got.At.Equal(at) {
		t.Errorf("Insert with a future arrival: want held by me at version 6, got %+v, %v", got, p.Err)
	}
}

func TestEvaluateDeleteMovesVersion(t *testing.T) {
	s := newStore(Lock{})
	if got := s.evaluate("me", doc("a", 5).Delete()).Locks[group]; got.Version != 6 {
		t.Errorf("Delete from an unheld group: want version 6, got %+v", got)
	}
}

func TestExclusive(t *testing.T) {
	s := newStore(Lock{})
	mod := entroq.NewModification("me",
		entroq.PuttingDocInto("ns", entroq.WithKeys("appended", "")),
		entroq.PuttingDocInto("ns", entroq.WithKeys("claimed", ""), entroq.WithDocArrivalTime(now)),
		doc("a", 5).Depend(),
	)
	got := Exclusive(mod, s.member)
	if len(got) != 1 || !got[Group{Namespace: "ns", Key: "claimed"}] {
		t.Errorf("Inserts and depends: want only the claiming insert's group exclusive, got %v", got)
	}
	mod = entroq.NewModification("me", doc("a", 5).Change(entroq.WithContent("x")))
	if got := Exclusive(mod, s.member); !got[group] {
		t.Errorf("Change: want its group exclusive, got %v", got)
	}
}

package worker

import (
	"slices"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// TestUntouchedReleasesOnlyWhatWasClaimed is a tripwire for the invariant in
// untouched's contract: the release list never names a doc set this worker did
// not claim. Releasing moves a set's version, so naming an unclaimed one would
// disturb a set that is somebody else's or nobody's.
//
// The invariant holds because untouched draws its result from the claimed sets
// and uses the modification only to exclude. An inversion of that -- computing
// the list from the sets the modification names -- would pass every other test
// in this package, which is why this one exists.
func TestUntouchedReleasesOnlyWhatWasClaimed(t *testing.T) {
	const ns = "ns"
	doc := func(id, key string) *entroq.Doc {
		return &entroq.Doc{Namespace: ns, ID: id, Key: key, Version: 3}
	}
	claimed := []*entroq.DocSet{
		{Namespace: ns, Key: "held-unmodified", Version: 3, Docs: []*entroq.Doc{doc("a", "held-unmodified")}},
		{Namespace: ns, Key: "held-empty", Version: 3},
		{Namespace: ns, Key: "held-modified", Version: 3, Docs: []*entroq.Doc{doc("b", "held-modified")}},
	}

	mod := entroq.NewModification("me",
		// Modifies a set we hold: the commit decided its arrival, so it stays
		// out of the release.
		doc("b", "held-modified").Change(entroq.WithDocArrivalTimeBy(time.Minute)),
		// Only watches a set we hold, which leaves it eligible.
		doc("a", "held-unmodified").Depend(),
		// Names sets never claimed here, every way a modification can. None of
		// these may come back, whatever the modification does with them.
		doc("z", "unclaimed-depend").Depend(),
		doc("y", "unclaimed-change").Change(entroq.WithContent("x")),
		entroq.PuttingDocInto(ns, entroq.WithKeys("unclaimed-insert", "")),
		entroq.Arriving(entroq.ReadyNow().Docs(
			&entroq.DocSet{Namespace: ns, Key: "unclaimed-arrival", Version: 3})),
	)

	got := untouched(mod, claimed)

	held := make(map[setKey]bool, len(claimed))
	for _, g := range claimed {
		held[setKey{g.Namespace, g.Key}] = true
	}
	for _, g := range got {
		if !held[setKey{g.Namespace, g.Key}] {
			t.Errorf("release list names %q in %q, which was never claimed here", g.Key, g.Namespace)
		}
	}

	var keys []string
	for _, g := range got {
		keys = append(keys, g.Key)
	}
	slices.Sort(keys)
	if want := []string{"held-empty", "held-unmodified"}; !slices.Equal(keys, want) {
		t.Errorf("release list is %v, want %v: of the sets claimed here, the ones not already modified", keys, want)
	}
}

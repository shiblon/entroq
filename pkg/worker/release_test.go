package worker

import (
	"slices"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// releasedKeys returns the set keys that releasing added to m, ignoring any
// arrival the modification already carried.
func releasedKeys(m *entroq.Modification, before int) []string {
	var keys []string
	for _, a := range m.DocArrives[before:] {
		keys = append(keys, a.Key)
	}
	slices.Sort(keys)
	return keys
}

// TestReleasingFreesWhatTheBodyDidNotKeep covers the rule in releasing's
// contract: a doc claim is a transaction scoped to the worker body, so every
// set claimed here goes back when the body ends, EXCEPT one the modification
// pushes into the future.
//
// The distinguishing case is held-modified-now. The old rule released only the
// sets a modification had not touched at all, so writing a member kept its set
// held whatever arrival the write asked for. Now only an arrival in the future
// keeps it, so a member written with no arrival releases like anything else.
func TestReleasingFreesWhatTheBodyDidNotKeep(t *testing.T) {
	const ns = "ns"
	doc := func(id, key string) *entroq.Doc {
		return &entroq.Doc{Namespace: ns, ID: id, Key: key, Version: 3}
	}
	claimed := []*entroq.DocSet{
		{Namespace: ns, Key: "held-unmodified", Version: 3, Docs: []*entroq.Doc{doc("a", "held-unmodified")}},
		{Namespace: ns, Key: "held-empty", Version: 3},
		{Namespace: ns, Key: "held-future", Version: 3, Docs: []*entroq.Doc{doc("b", "held-future")}},
		{Namespace: ns, Key: "held-modified-now", Version: 3, Docs: []*entroq.Doc{doc("c", "held-modified-now")}},
	}

	mod := entroq.NewModification("me",
		// Pushed into the future: explicit intent to keep holding it.
		doc("b", "held-future").Change(entroq.WithDocArrivalTimeBy(time.Minute)),
		// Written, but asking no arrival, so the body is done with it.
		doc("c", "held-modified-now").Change(entroq.WithContent("x")),
		// Only watched, which says nothing about wanting to keep it.
		doc("a", "held-unmodified").Depend(),
		// Sets never claimed here, every way a modification can name one. None
		// of these may come back, whatever the modification does with them.
		doc("z", "unclaimed-depend").Depend(),
		doc("y", "unclaimed-change").Change(entroq.WithContent("x")),
		entroq.PuttingDocInto(ns, entroq.WithKeys("unclaimed-insert", "")),
		entroq.Arriving(entroq.ReadyNow().Docs(
			&entroq.DocSet{Namespace: ns, Key: "unclaimed-arrival", Version: 3})),
	)
	before := len(mod.DocArrives)

	releasing(mod, claimed)

	held := make(map[setKey]bool, len(claimed))
	for _, g := range claimed {
		held[setKey{g.Namespace, g.Key}] = true
	}
	for _, a := range mod.DocArrives[before:] {
		if !held[setKey{a.Namespace, a.Key}] {
			t.Errorf("released %q in %q, which was never claimed here", a.Key, a.Namespace)
		}
	}

	want := []string{"held-empty", "held-modified-now", "held-unmodified"}
	if got := releasedKeys(mod, before); !slices.Equal(got, want) {
		t.Errorf("released %v, want %v", got, want)
	}
}

// TestReleasingSkipsASetTheBodyAlreadyNamed keeps releasing from doubling an
// arrival the handler wrote itself. The handler decided, whichever way it
// decided, so there is nothing to add.
func TestReleasingSkipsASetTheBodyAlreadyNamed(t *testing.T) {
	const ns = "ns"
	claimed := []*entroq.DocSet{
		{Namespace: ns, Key: "released-by-hand", Version: 3},
		{Namespace: ns, Key: "renewed-by-hand", Version: 3},
	}
	mod := entroq.NewModification("me",
		entroq.Arriving(
			entroq.ReadyNow().Docs(&entroq.DocSet{Namespace: ns, Key: "released-by-hand", Version: 3}),
			entroq.ReadyIn(time.Minute).Docs(&entroq.DocSet{Namespace: ns, Key: "renewed-by-hand", Version: 3}),
		),
	)
	before := len(mod.DocArrives)

	releasing(mod, claimed)

	if got := releasedKeys(mod, before); len(got) != 0 {
		t.Errorf("released %v, want nothing: both sets were already named", got)
	}
}

// TestReleasingAnEmptyLeaseholdWritesNothing keeps a body that claimed no sets
// from turning into a modification. Modify refuses one naming no operation, so
// a handler returning nothing at all must not produce a call.
func TestReleasingAnEmptyLeaseholdWritesNothing(t *testing.T) {
	mod := entroq.NewModification("me")

	releasing(mod, nil)

	if !mod.IsEmpty() {
		t.Errorf("releasing with no claimed sets built %v, want nothing", mod)
	}
}

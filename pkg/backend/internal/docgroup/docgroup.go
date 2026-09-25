// Package docgroup holds the claim and version rules for doc groups, which
// every backend applies the same way.
//
// A doc group is the set of docs sharing a primary key in a namespace. It
// behaves like a task whose members share one lifecycle: the group has a
// single lock carrying the only version, claimant, and arrival time its
// members have. Claiming, renewing, and changing or deleting a member move
// that version, and while one claimant holds the group nobody else may write
// to it.
//
// The version moves for exactly the writes that can falsify something a
// reader saw at it. Inserting into an unheld group adds a member without
// changing any other, so it leaves the version alone and inserts do not
// contend with each other. A delete does move it, or deleting a member and
// inserting another with the same ID would change content at one version.
// Only a claim tells a reader the group's membership is complete.
package docgroup

import (
	"time"

	"github.com/shiblon/entroq"
)

// Group names a doc group.
type Group struct {
	Namespace string
	Key       string
}

// Lock is a group's version and claim.
type Lock struct {
	Version  int32
	Claimant string
	At       time.Time
}

// Absent is the lock of a group nothing has written or claimed yet. Its first
// write or claim moves it to version 0, so a new doc starts at version 0 as a
// new task does. Backends return it for a group they have no lock for.
var Absent = Lock{Version: -1}

// New reports whether l is a group nothing has written or claimed yet. Only
// the version tells: a backend that creates a lock row before deciding what
// to write gives it a time.
func (l Lock) New() bool {
	return l.Version == Absent.Version
}

// Held reports whether anyone holds the lock at now.
func (l Lock) Held(now time.Time) bool {
	return l.Claimant != "" && now.Before(l.At)
}

// HeldByOther reports whether someone other than claimant holds the lock at
// now.
func (l Lock) HeldByOther(claimant string, now time.Time) bool {
	return l.Held(now) && l.Claimant != claimant
}

// Overlay returns a copy of member carrying its group's version and claim, the
// only ones a member has.
func Overlay(member *entroq.Doc, l Lock) *entroq.Doc {
	d := member.Copy()
	d.Version = l.Version
	d.Claimant = l.Claimant
	d.At = l.At
	return d
}

// Claim returns l claimed by claimant until now+d, or false if someone else
// holds it. Claiming moves the version, so any earlier read of the group is
// stale; the holder claiming again extends its claim the same way.
func Claim(l Lock, claimant string, now time.Time, d time.Duration) (Lock, bool) {
	if l.HeldByOther(claimant, now) {
		return l, false
	}
	return Lock{Version: l.Version + 1, Claimant: claimant, At: now.Add(d)}, true
}

// Plan is what a modification does to doc groups.
type Plan struct {
	// Err describes every doc operation that cannot proceed, or is nil.
	Err *entroq.DependencyError
	// Locks holds the new lock of every group the modification writes.
	Locks map[Group]Lock
}

// Evaluate checks mod's doc operations and computes the lock each written
// group ends with. member returns the stored doc with the given namespace and
// ID, or nil; lock returns a group's current lock, or Absent.
//
// A change, delete, or depend names a member at its group's version; the
// member's stored key, not the one in the request, decides its group. A change
// or delete also fails while someone else holds the group, and so does an
// insert, which checks no version. A depend only reads, so it neither checks
// the claim nor writes the group.
//
// An insert writes its group only to create it, at version 0, to claim it by
// carrying a future arrival time, or when mod.Claimant already holds it, whose
// commit releases or renews the claim. Otherwise the group's lock is left as
// it is.
//
// Each written group's version moves once. If any of its changes or inserts
// carries a future arrival time, the group ends held by mod.Claimant until the
// latest one; otherwise the write releases it, as committing a task releases
// its claim.
func Evaluate(mod *entroq.Modification, now time.Time, member func(ns, id string) *entroq.Doc, lock func(Group) Lock) Plan {
	depErr := new(entroq.DependencyError)
	held := make(map[Group]time.Time) // written groups, with the latest future arrival

	write := func(g Group, at time.Time) {
		latest := held[g]
		if at.After(now) && at.After(latest) {
			latest = at
		}
		held[g] = latest
	}
	claimed := func(g Group) bool {
		return lock(g).HeldByOther(mod.Claimant, now)
	}
	// stored returns the member's group and lock, or false if it does not
	// exist at version.
	stored := func(ns, id string, version int32) (Group, bool) {
		d := member(ns, id)
		if d == nil {
			return Group{}, false
		}
		g := Group{Namespace: d.Namespace, Key: d.Key}
		return g, lock(g).Version == version
	}

	for _, ins := range mod.DocInserts {
		id := entroq.NewDocID(ins.Namespace, ins.ID, 0)
		if ins.ID != "" && member(ins.Namespace, ins.ID) != nil {
			depErr.DocInserts = append(depErr.DocInserts, id)
			continue
		}
		g := Group{Namespace: ins.Namespace, Key: ins.Key}
		switch l := lock(g); {
		case claimed(g):
			id.Version = l.Version
			depErr.DocClaims = append(depErr.DocClaims, id)
		case l.New() || l.Held(now) || ins.At.After(now):
			write(g, ins.At)
		}
	}
	for _, chg := range mod.DocChanges {
		id := entroq.NewDocID(chg.Namespace, chg.ID, chg.Version)
		g, ok := stored(chg.Namespace, chg.ID, chg.Version)
		switch {
		case !ok:
			depErr.DocChanges = append(depErr.DocChanges, id)
		case claimed(g):
			depErr.DocClaims = append(depErr.DocClaims, id)
		default:
			write(g, chg.At)
		}
	}
	for _, del := range mod.DocDeletes {
		g, ok := stored(del.Namespace, del.ID, del.Version)
		switch {
		case !ok:
			depErr.DocDeletes = append(depErr.DocDeletes, del)
		case claimed(g):
			depErr.DocClaims = append(depErr.DocClaims, del)
		default:
			write(g, time.Time{})
		}
	}
	for _, dep := range mod.DocDepends {
		if _, ok := stored(dep.Namespace, dep.ID, dep.Version); !ok {
			depErr.DocDepends = append(depErr.DocDepends, dep)
		}
	}

	plan := Plan{Locks: make(map[Group]Lock, len(held))}
	if depErr.HasAny() {
		plan.Err = depErr
		return plan
	}
	for g, at := range held {
		next := Lock{Version: lock(g).Version + 1, At: now}
		if !at.IsZero() {
			next.Claimant = mod.Claimant
			next.At = at
		}
		plan.Locks[g] = next
	}
	return plan
}

// Groups lists the doc groups mod's doc operations name, each once: an
// insert's by its key, and every other operation's by its stored member's
// key. member returns the stored doc with the given namespace and ID, or nil.
// A backend locks or reads these groups before calling Evaluate; see
// Exclusive for which need an exclusive lock.
func Groups(mod *entroq.Modification, member func(ns, id string) *entroq.Doc) []Group {
	seen := make(map[Group]bool)
	var groups []Group
	add := func(g Group) {
		if !seen[g] {
			seen[g] = true
			groups = append(groups, g)
		}
	}
	for _, ins := range mod.DocInserts {
		add(Group{Namespace: ins.Namespace, Key: ins.Key})
	}
	stored := func(ns, id string) {
		if d := member(ns, id); d != nil {
			add(Group{Namespace: d.Namespace, Key: d.Key})
		}
	}
	for _, chg := range mod.DocChanges {
		stored(chg.Namespace, chg.ID)
	}
	for _, del := range mod.DocDeletes {
		stored(del.Namespace, del.ID)
	}
	for _, dep := range mod.DocDepends {
		stored(dep.Namespace, dep.ID)
	}
	return groups
}

// Exclusive reports which of mod's groups a backend must lock exclusively
// before calling Evaluate: those mod changes or deletes a member of, and those
// an insert may claim by carrying an arrival time. mod only inserts into or
// depends on the rest, so they may be locked shared: concurrent inserts can
// proceed together, while a claim or lock collection waits for them. Evaluate
// still writes a shared group when mod.Claimant holds it or it is new, so the
// backend must be able to take it exclusively then.
func Exclusive(mod *entroq.Modification, member func(ns, id string) *entroq.Doc) map[Group]bool {
	exclusive := make(map[Group]bool)
	for _, ins := range mod.DocInserts {
		if !ins.At.IsZero() {
			exclusive[Group{Namespace: ins.Namespace, Key: ins.Key}] = true
		}
	}
	stored := func(ns, id string) {
		if d := member(ns, id); d != nil {
			exclusive[Group{Namespace: d.Namespace, Key: d.Key}] = true
		}
	}
	for _, chg := range mod.DocChanges {
		stored(chg.Namespace, chg.ID)
	}
	for _, del := range mod.DocDeletes {
		stored(del.Namespace, del.ID)
	}
	return exclusive
}

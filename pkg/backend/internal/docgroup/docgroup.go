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
// Every write to a group moves its version, inserts included, so a version a
// reader saw means the group, members and all, is as the reader saw it.
// Concurrent writers of one group therefore contend; a workload with many
// writers is better spread over several primary keys.
package docgroup

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
)

// Group names a doc group.
type Group struct {
	Namespace string
	Key       string
}

// Lock is a group's version and claim, and how many docs it has.
type Lock struct {
	Version  int32
	Claimant string
	At       time.Time
	NumDocs  int
}

// Absent is the lock of a group nothing has written or claimed yet. Its first
// write or claim moves it to version 0, so a new doc starts at version 0 as a
// new task does. Backends return it for a group they have no lock for.
var Absent = Lock{Version: -1}

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
	return Lock{Version: l.Version + 1, Claimant: claimant, At: now.Add(d), NumDocs: l.NumDocs}, true
}

// Claimed returns the group g as its claim left it: holding lock l, with each
// member carrying that lock.
func Claimed(g Group, l Lock, members []*entroq.Doc) *entroq.DocGroup {
	dg := &entroq.DocGroup{
		Namespace: g.Namespace,
		Key:       g.Key,
		Version:   l.Version,
		Claimant:  l.Claimant,
		At:        l.At,
		NumDocs:   l.NumDocs,
		Docs:      make([]*entroq.Doc, 0, len(members)),
	}
	for _, d := range members {
		dg.Docs = append(dg.Docs, Overlay(d, l))
	}
	return dg
}

// Current returns the group g as its lock l stands, without its docs: what a
// dependency error or an arrival update reports.
func Current(g Group, l Lock) *entroq.DocGroup {
	return &entroq.DocGroup{
		Namespace: g.Namespace,
		Key:       g.Key,
		Version:   l.Version,
		Claimant:  l.Claimant,
		At:        l.At,
		NumDocs:   l.NumDocs,
	}
}

// HeldError is the error for a claim of g, holding lock l, by someone else:
// it names the group, with its lock, and its members, for clients that know
// only doc failures.
func HeldError(g Group, l Lock, members []*entroq.Doc) *entroq.DependencyError {
	depErr := &entroq.DependencyError{
		Message:     fmt.Sprintf("doc group %q in namespace %q is claimed by %s until %v", g.Key, g.Namespace, l.Claimant, l.At),
		GroupClaims: []*entroq.DocGroup{Current(g, l)},
	}
	for _, d := range members {
		depErr.DocClaims = append(depErr.DocClaims, entroq.NewDocID(d.Namespace, d.ID, l.Version))
	}
	return depErr
}

// Plan is what a modification does to doc groups.
type Plan struct {
	// Err describes every doc operation that cannot proceed, or is nil.
	Err *entroq.DependencyError
	// Locks holds the new lock of every group the modification writes.
	Locks map[Group]Lock
	// Arrived holds each group mod.DocArrives names at its new lock, in the
	// order named: a modify response's changed groups.
	Arrived []*entroq.DocGroup
}

// Evaluate checks mod's doc operations and computes the lock each written
// group ends with, its doc count moved by the docs the modification inserts
// and deletes. member returns the stored doc with the given namespace and ID,
// or nil; lock returns a group's current lock, or Absent.
//
// A change, delete, or depend names a member at its group's version; the
// member's stored key, not the one in the request, decides its group. A change
// or delete also fails while someone else holds the group, and so does an
// insert, which checks no version. A depend only reads, so it neither checks
// the claim nor writes the group. Changes, deletes, and inserts write it.
//
// Each written group's version moves once. If any of its changes or inserts
// carries a future arrival time, the group ends held by mod.Claimant until the
// latest one; otherwise the write releases it, as committing a task releases
// its claim.
func Evaluate(mod *entroq.Modification, now time.Time, member func(ns, id string) *entroq.Doc, lock func(Group) Lock) Plan {
	depErr := new(entroq.DependencyError)
	held := make(map[Group]time.Time) // written groups, with the latest future arrival
	added := make(map[Group]int)      // docs inserted less docs deleted, per group

	write := func(g Group, at time.Time) {
		latest := held[g]
		if at.After(now) && at.After(latest) {
			latest = at
		}
		held[g] = latest
	}
	// claimed reports whether someone else holds g, naming the group in the
	// error the first time. The members are named too, for clients that
	// know only doc failures.
	reported := make(map[Group]bool)
	claimed := func(g Group) bool {
		l := lock(g)
		if !l.HeldByOther(mod.Claimant, now) {
			return false
		}
		if !reported[g] {
			reported[g] = true
			depErr.GroupClaims = append(depErr.GroupClaims, Current(g, l))
		}
		return true
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
		if claimed(g) {
			id.Version = lock(g).Version
			depErr.DocClaims = append(depErr.DocClaims, id)
			continue
		}
		write(g, ins.At)
		added[g]++
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
			added[g]--
		}
	}
	for _, dep := range mod.DocDepends {
		if _, ok := stored(dep.Namespace, dep.ID, dep.Version); !ok {
			depErr.DocDepends = append(depErr.DocDepends, dep)
		}
	}
	// An arrival names the group itself, at its version, and writes only when
	// it is ready again.
	for _, a := range mod.DocArrives {
		g := Group{Namespace: a.Namespace, Key: a.Key}
		l := lock(g)
		switch {
		case l.Version < 0 || l.Version != a.Version:
			depErr.DocArrives = append(depErr.DocArrives, Current(g, l))
		case claimed(g):
		default:
			write(g, a.At)
		}
	}

	plan := Plan{Locks: make(map[Group]Lock, len(held))}
	if depErr.HasAny() {
		plan.Err = depErr
		return plan
	}
	for g, at := range held {
		next := Lock{Version: lock(g).Version + 1, At: now, NumDocs: lock(g).NumDocs + added[g]}
		if !at.IsZero() {
			next.Claimant = mod.Claimant
			next.At = at
		}
		plan.Locks[g] = next
	}
	for _, a := range mod.DocArrives {
		g := Group{Namespace: a.Namespace, Key: a.Key}
		plan.Arrived = append(plan.Arrived, Current(g, plan.Locks[g]))
	}
	return plan
}

// Groups lists the doc groups mod's doc operations name, each once: an
// insert's and an arrival's by its key, and every other operation's by its
// stored member's key. member returns the stored doc with the given namespace and ID, or nil.
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
	for _, a := range mod.DocArrives {
		add(Group{Namespace: a.Namespace, Key: a.Key})
	}
	return groups
}

// Exclusive reports which of mod's groups a backend must lock exclusively
// before calling Evaluate: those it writes, by inserting into them, changing
// or deleting a member, or changing their arrival. mod only depends on the
// rest, so they may be locked
// shared, and since a depend names a stored member, they already exist.
func Exclusive(mod *entroq.Modification, member func(ns, id string) *entroq.Doc) map[Group]bool {
	exclusive := make(map[Group]bool)
	for _, ins := range mod.DocInserts {
		exclusive[Group{Namespace: ins.Namespace, Key: ins.Key}] = true
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
	for _, a := range mod.DocArrives {
		exclusive[Group{Namespace: a.Namespace, Key: a.Key}] = true
	}
	return exclusive
}

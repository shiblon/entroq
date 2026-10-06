// Package docset holds the claim and version rules for doc sets, which
// every backend applies the same way.
//
// A doc set is the set of docs sharing a primary key in a namespace. It
// behaves like a task whose members share one lifecycle: the set has a
// single lock carrying the only version, claimant, and arrival time its
// members have. Claiming, renewing, and changing or deleting a member move
// that version, and while one claimant holds the set nobody else may write
// to it.
//
// Every write to a set moves its version, inserts included, so a version a
// reader saw means the set, members and all, is as the reader saw it.
// Concurrent writers of one set therefore contend; a workload with many
// writers is better spread over several primary keys.
package docset

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
)

// Set names a doc set.
type Set struct {
	Namespace string
	Key       string
}

// Lock is a set's version and claim, and how many docs it has. Stored says
// the set has a lock of its own; see Absent.
type Lock struct {
	Stored   bool
	Version  int32
	Claimant string
	At       time.Time
	NumDocs  int
}

// Absent is the lock of a set nothing has stored: version 0, no docs, no
// claim. Backends return it for a set they have no lock for.
//
// An empty set and an absent one are the same state, so version 0 is the
// version an empty set has: the set that was, in effect, inserted empty.
// Stored is what tells them apart, and only an insert needs to know, because
// only an insert can bring a set into being. Nothing reads Version to decide
// whether a set exists -- 0 is a real version a stored set holds.
var Absent = Lock{}

// Held reports whether anyone holds the lock at now.
func (l Lock) Held(now time.Time) bool {
	return l.Claimant != "" && now.Before(l.At)
}

// HeldByOther reports whether someone other than claimant holds the lock at
// now.
func (l Lock) HeldByOther(claimant string, now time.Time) bool {
	return l.Held(now) && l.Claimant != claimant
}

// Overlay returns a copy of member carrying its set's version and claim, the
// only ones a member has.
func Overlay(member *entroq.Doc, l Lock) *entroq.Doc {
	d := member.Copy()
	d.Version = l.Version
	d.Claimant = l.Claimant
	d.At = l.At
	return d
}

// Claim returns lock l claimed by claimant until until, or false if someone
// else holds it at now.
//
// A claim moves the version like any other write, an absent set included: the
// set it claims is the empty one that was already there at version 0, so
// claiming it reads as the modification it is and lands at version 1. Only an
// insert creates a set, and only an insert starts one at version 0.
func Claim(l Lock, claimant string, now, until time.Time) (Lock, bool) {
	if l.HeldByOther(claimant, now) {
		return l, false
	}
	return Lock{Stored: true, Version: l.Version + 1, Claimant: claimant, At: until, NumDocs: l.NumDocs}, true
}

// written returns the version a write to a set holding lock l produces: a
// write to a set nothing has stored creates it at version 0, as inserting a
// task creates it at version 0, and a write to one already there moves it on
// by one.
func written(l Lock) int32 {
	if !l.Stored {
		return 0
	}
	return l.Version + 1
}

// SetsOf returns the sets cq names, in the order named.
func SetsOf(cq *entroq.DocClaim) []Set {
	sets := make([]Set, len(cq.Sets))
	for i, s := range cq.Sets {
		sets[i] = Set{Namespace: s.Namespace, Key: s.Key}
	}
	return sets
}

// MissingTaskErrorf reports that a claim matching a task
// (entroq.MatchingLeaseOf) could not find it at the version named. The claim
// depends on that task, so this is an ordinary failed depend: a
// DependencyError with the task among Depends, where Error renders it under
// "missing depends".
//
// The origin goes in the message rather than wrapping it: only
// DependencyError.Message crosses the wire, so a fmt.Errorf wrap would name
// the backend for a local caller and tell a remote one nothing.
func MissingTaskErrorf(id *entroq.TaskID, format string, args ...any) error {
	depErr := entroq.DependencyErrorf("%s: doc claim depends on a missing task",
		fmt.Sprintf(format, args...))
	depErr.Depends = append(depErr.Depends, id)
	return depErr
}

// ClaimAll claims every set cq names, all or none, at now, holding them until
// until, and returns each set's new lock in the order named. lock gives each
// set's current lock; members gives a set's docs, and is asked only for sets
// held by someone else, which the error names along with their members. A
// hold that does not reach past now is an invalid argument.
//
// until is passed in rather than read from cq because a claim may name a task
// to match instead of a duration (see entroq.MatchingLeaseOf), and only the
// backend can resolve that, by reading the task's own stored arrival.
//
// Read that task optimistically; do not lock or WATCH it. A stale read leaves
// the hold wrong by at most one lease, which the holder's next renewal
// corrects, and watching it makes the claim retry whenever the task is
// touched, including by its own holder. If it is missing at the version named,
// fail with MissingTaskErrorf.
func ClaimAll(cq *entroq.DocClaim, now, until time.Time, lock func(Set) Lock, members func(Set) ([]*entroq.Doc, error)) ([]Lock, error) {
	if !until.After(now) {
		return nil, entroq.InvalidArgumentf("doc claim until %v, which is not after now (%v)", until, now)
	}
	sets := SetsOf(cq)
	claimed := make([]Lock, len(sets))
	var held *entroq.DependencyError
	for i, g := range sets {
		l := lock(g)
		next, ok := Claim(l, cq.Claimant, now, until)
		if ok {
			claimed[i] = next
			continue
		}
		ms, err := members(g)
		if err != nil {
			return nil, err
		}
		if held == nil {
			held = HeldError(g, l, ms)
		} else {
			held = held.Merge(HeldError(g, l, ms))
		}
	}
	if held != nil {
		return nil, held
	}
	return claimed, nil
}

// ClaimedSets returns what a claim of cq returns: each set at its new lock,
// in locks, in the order named, with its members unless it was claimed
// OmitMembers. members is asked only for the sets whose docs come back.
func ClaimedSets(cq *entroq.DocClaim, locks []Lock, members func(Set) ([]*entroq.Doc, error)) ([]*entroq.DocSet, error) {
	out := make([]*entroq.DocSet, len(cq.Sets))
	for i, g := range SetsOf(cq) {
		if cq.Sets[i].OmitMembers {
			out[i] = Current(g, locks[i])
			continue
		}
		ms, err := members(g)
		if err != nil {
			return nil, err
		}
		out[i] = Claimed(g, locks[i], ms)
	}
	return out, nil
}

// Claimed returns the set g as its claim left it: holding lock l, with each
// member carrying that lock.
func Claimed(g Set, l Lock, members []*entroq.Doc) *entroq.DocSet {
	dg := &entroq.DocSet{
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

// Current returns the set g as its lock l stands, without its docs: what an
// arrival update reports.
func Current(g Set, l Lock) *entroq.DocSet {
	return &entroq.DocSet{
		Namespace: g.Namespace,
		Key:       g.Key,
		Version:   l.Version,
		Claimant:  l.Claimant,
		At:        l.At,
		NumDocs:   l.NumDocs,
	}
}

// Ref names the set g at its lock l's version, as a dependency error does.
func Ref(g Set, l Lock) *entroq.DocID {
	return entroq.NewDocSetRef(g.Namespace, g.Key, l.Version)
}

// HeldError is the error for a claim of g, holding lock l, by someone else:
// it names the set and then its members, for clients that know only doc
// failures.
func HeldError(g Set, l Lock, members []*entroq.Doc) *entroq.DependencyError {
	depErr := &entroq.DependencyError{
		Message:   fmt.Sprintf("doc set %q in namespace %q is claimed by %s until %v", g.Key, g.Namespace, l.Claimant, l.At),
		DocClaims: []*entroq.DocID{Ref(g, l)},
	}
	for _, d := range members {
		depErr.DocClaims = append(depErr.DocClaims, entroq.NewDocID(d.Namespace, d.ID, l.Version))
	}
	return depErr
}

// Plan is what a modification does to doc sets.
type Plan struct {
	// Err describes every doc operation that cannot proceed, or is nil.
	Err *entroq.DependencyError
	// Locks holds the new lock of every set the modification writes.
	Locks map[Set]Lock
	// Arrived holds each set mod.DocArrives names at its new lock, in the
	// order named: a modify response's changed sets.
	Arrived []*entroq.DocSet
}

// Evaluate checks mod's doc operations and computes the lock each written
// set ends with, its doc count moved by the docs the modification inserts
// and deletes. member returns the stored doc with the given namespace and ID,
// or nil; lock returns a set's current lock, or Absent.
//
// A change, delete, or depend names a member at its set's version; the
// member's stored key, not the one in the request, decides its set. A change
// or delete also fails while someone else holds the set, and so does an
// insert, which checks no version. A depend only reads, so it neither checks
// the claim nor writes the set. Changes, deletes, and inserts write it.
//
// Each written set's version moves once. If any of its changes or inserts
// carries a future arrival time, the set ends held by mod.Claimant until the
// latest one; otherwise the write releases it, as committing a task releases
// its claim.
func Evaluate(mod *entroq.Modification, now time.Time, member func(ns, id string) *entroq.Doc, lock func(Set) Lock) Plan {
	depErr := new(entroq.DependencyError)
	held := make(map[Set]time.Time) // written sets, with the latest future arrival
	added := make(map[Set]int)      // docs inserted less docs deleted, per set

	// write records that mod writes g, holding it until the latest arrival any
	// of those writes asks for. An arrival is named only as a duration from
	// now, so whether this holds or releases is the SIGN of that duration and
	// nothing else. That matters: when an arrival arrived as an instant
	// computed by the caller, one that went stale in flight was no longer
	// after now, which read as a release -- so a renewal merely delayed would
	// hand the set away and report success doing it.
	write := func(g Set, by time.Duration) {
		latest := held[g]
		if at := now.Add(by); by > 0 && at.After(latest) {
			latest = at
		}
		held[g] = latest
	}
	// claimed reports whether someone else holds g, naming the set in the
	// error the first time. The members are named too, for clients that
	// know only doc failures.
	reported := make(map[Set]bool)
	claimed := func(g Set) bool {
		l := lock(g)
		if !l.HeldByOther(mod.Claimant, now) {
			return false
		}
		if !reported[g] {
			reported[g] = true
			depErr.DocClaims = append(depErr.DocClaims, Ref(g, l))
		}
		return true
	}
	// stored returns the member's set and lock, or false if it does not
	// exist at version.
	stored := func(ns, id string, version int32) (Set, bool) {
		d := member(ns, id)
		if d == nil {
			return Set{}, false
		}
		g := Set{Namespace: d.Namespace, Key: d.Key}
		return g, lock(g).Version == version
	}

	for _, ins := range mod.DocInserts {
		id := entroq.NewDocID(ins.Namespace, ins.ID, 0)
		if ins.ID != "" && member(ins.Namespace, ins.ID) != nil {
			depErr.DocInserts = append(depErr.DocInserts, id)
			continue
		}
		g := Set{Namespace: ins.Namespace, Key: ins.Key}
		if claimed(g) {
			id.Version = lock(g).Version
			depErr.DocClaims = append(depErr.DocClaims, id)
			continue
		}
		write(g, ins.By())
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
			write(g, chg.By())
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
			write(g, 0) // a delete never holds its set
			added[g]--
		}
	}
	// A depend naming a SET rather than a member watches the set itself at its
	// own version: the only version its members have. This is how to depend on
	// something you do NOT hold -- a shared config set read and acted on, where
	// the commit must fail if it moved underneath -- since there is no claim to
	// lean on there.
	//
	// The set must be stored. Version 0 is a real version a stored set holds,
	// so it cannot stand for "not there"; and a set with no lock row is one no
	// shared lock can hold still, so a depend on it could not be enforced
	// against a concurrent insert even if it were allowed to pass.
	for _, dep := range mod.DocDepends {
		if dep.IsSetRef() {
			l := lock(Set{Namespace: dep.Namespace, Key: dep.Key})
			if !l.Stored || l.Version != dep.Version {
				depErr.DocDepends = append(depErr.DocDepends, dep)
			}
			continue
		}
		if _, ok := stored(dep.Namespace, dep.ID, dep.Version); !ok {
			depErr.DocDepends = append(depErr.DocDepends, dep)
		}
	}
	// An arrival names the set itself, at its version, and writes only when
	// it is ready again.
	for _, a := range mod.DocArrives {
		g := Set{Namespace: a.Namespace, Key: a.Key}
		l := lock(g)
		switch {
		case !l.Stored || l.Version != a.Version:
			depErr.DocArrives = append(depErr.DocArrives, Ref(g, l))
		case claimed(g):
		default:
			write(g, a.By)
		}
	}

	plan := Plan{Locks: make(map[Set]Lock, len(held))}
	if depErr.HasAny() {
		plan.Err = depErr
		return plan
	}
	for g, at := range held {
		next := Lock{Stored: true, Version: written(lock(g)), At: now, NumDocs: lock(g).NumDocs + added[g]}
		if !at.IsZero() {
			next.Claimant = mod.Claimant
			next.At = at
		}
		plan.Locks[g] = next
	}
	for _, a := range mod.DocArrives {
		g := Set{Namespace: a.Namespace, Key: a.Key}
		plan.Arrived = append(plan.Arrived, Current(g, plan.Locks[g]))
	}
	return plan
}

// Sets lists the doc sets mod's doc operations name, each once: an insert's, an
// arrival's, and a depend naming a set by its key, and every other operation's
// by its stored member's key. member returns the stored doc with the given
// namespace and ID, or nil.
// A backend locks or reads these sets before calling Evaluate; see
// Exclusive for which need an exclusive lock.
func Sets(mod *entroq.Modification, member func(ns, id string) *entroq.Doc) []Set {
	seen := make(map[Set]bool)
	var sets []Set
	add := func(g Set) {
		if !seen[g] {
			seen[g] = true
			sets = append(sets, g)
		}
	}
	for _, ins := range mod.DocInserts {
		add(Set{Namespace: ins.Namespace, Key: ins.Key})
	}
	stored := func(ns, id string) {
		if d := member(ns, id); d != nil {
			add(Set{Namespace: d.Namespace, Key: d.Key})
		}
	}
	for _, chg := range mod.DocChanges {
		stored(chg.Namespace, chg.ID)
	}
	for _, del := range mod.DocDeletes {
		stored(del.Namespace, del.ID)
	}
	for _, dep := range mod.DocDepends {
		if dep.IsSetRef() {
			add(Set{Namespace: dep.Namespace, Key: dep.Key})
			continue
		}
		stored(dep.Namespace, dep.ID)
	}
	for _, a := range mod.DocArrives {
		add(Set{Namespace: a.Namespace, Key: a.Key})
	}
	return sets
}

// Exclusive reports which of mod's sets a backend must lock exclusively before
// calling Evaluate: those it writes, by inserting into them, changing or
// deleting a member, or changing their arrival. mod only depends on the rest, so
// those may be locked shared.
//
// A depend never appears here, however it names its set. A set it names by a
// stored member exists by definition; one it names by key may not, and Evaluate
// fails such a depend rather than creating a row to lock, so a shared lock over
// what is there is sufficient either way.
func Exclusive(mod *entroq.Modification, member func(ns, id string) *entroq.Doc) map[Set]bool {
	exclusive := make(map[Set]bool)
	for _, ins := range mod.DocInserts {
		exclusive[Set{Namespace: ins.Namespace, Key: ins.Key}] = true
	}
	stored := func(ns, id string) {
		if d := member(ns, id); d != nil {
			exclusive[Set{Namespace: d.Namespace, Key: d.Key}] = true
		}
	}
	for _, chg := range mod.DocChanges {
		stored(chg.Namespace, chg.ID)
	}
	for _, del := range mod.DocDeletes {
		stored(del.Namespace, del.ID)
	}
	for _, a := range mod.DocArrives {
		exclusive[Set{Namespace: a.Namespace, Key: a.Key}] = true
	}
	return exclusive
}

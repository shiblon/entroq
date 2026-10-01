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

// Lock is a set's version and claim, and how many docs it has.
type Lock struct {
	Version  int32
	Claimant string
	At       time.Time
	NumDocs  int
}

// Absent is the lock of a set nothing has written or claimed yet. Its first
// write or claim moves it to version 0, so a new doc starts at version 0 as a
// new task does. Backends return it for a set they have no lock for.
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
func Claim(l Lock, claimant string, now, until time.Time) (Lock, bool) {
	if l.HeldByOther(claimant, now) {
		return l, false
	}
	return Lock{Version: l.Version + 1, Claimant: claimant, At: until, NumDocs: l.NumDocs}, true
}

// SetsOf returns the sets cq names, in the order named.
func SetsOf(cq *entroq.DocClaim) []Set {
	sets := make([]Set, len(cq.Sets))
	for i, s := range cq.Sets {
		sets[i] = Set{Namespace: s.Namespace, Key: s.Key}
	}
	return sets
}

// ClaimAll claims every set cq names, all or none, at now, and returns each
// set's new lock in the order named. lock gives each set's current lock;
// members gives a set's docs, and is asked only for sets held by someone
// else, which the error names along with their members. A claim whose time
// is not after now is an invalid argument.
func ClaimAll(cq *entroq.DocClaim, now time.Time, lock func(Set) Lock, members func(Set) ([]*entroq.Doc, error)) ([]Lock, error) {
	until := cq.Until(now)
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

	write := func(g Set, at time.Time) {
		latest := held[g]
		if at.After(now) && at.After(latest) {
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
	// An arrival names the set itself, at its version, and writes only when
	// it is ready again.
	for _, a := range mod.DocArrives {
		g := Set{Namespace: a.Namespace, Key: a.Key}
		l := lock(g)
		switch {
		case l.Version < 0 || l.Version != a.Version:
			depErr.DocArrives = append(depErr.DocArrives, Ref(g, l))
		case claimed(g):
		default:
			write(g, a.At)
		}
	}

	plan := Plan{Locks: make(map[Set]Lock, len(held))}
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
		g := Set{Namespace: a.Namespace, Key: a.Key}
		plan.Arrived = append(plan.Arrived, Current(g, plan.Locks[g]))
	}
	return plan
}

// Sets lists the doc sets mod's doc operations name, each once: an
// insert's and an arrival's by its key, and every other operation's by its
// stored member's key. member returns the stored doc with the given namespace and ID, or nil.
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
		stored(dep.Namespace, dep.ID)
	}
	for _, a := range mod.DocArrives {
		add(Set{Namespace: a.Namespace, Key: a.Key})
	}
	return sets
}

// Exclusive reports which of mod's sets a backend must lock exclusively
// before calling Evaluate: those it writes, by inserting into them, changing
// or deleting a member, or changing their arrival. mod only depends on the
// rest, so they may be locked
// shared, and since a depend names a stored member, they already exist.
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

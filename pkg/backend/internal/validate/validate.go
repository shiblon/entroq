// Package validate holds the checks every backend applies to a request before
// touching storage, so all of them reject the same requests with the same
// entroq.InvalidArgumentError rather than a driver-specific failure.
//
// Byte-length bounds match the PostgreSQL and SQLite schemas' CHECK
// constraints, which the SQL backends keep as a second line of defense. Every
// bound counts bytes (Go's len), matching octet_length in SQL. Queue names are
// deliberately unbounded.
package validate

import (
	"github.com/shiblon/entroq"
)

const (
	// MaxIDBytes bounds task and doc IDs.
	MaxIDBytes = 64
	// MaxClaimantBytes bounds task and doc claimants.
	MaxClaimantBytes = 64
	// MaxNamespaceBytes bounds doc namespaces.
	MaxNamespaceBytes = 1024
	// MaxDocKeyBytes bounds doc primary and secondary keys.
	MaxDocKeyBytes = 256
)

func check(what, s string, limit int) error {
	if len(s) > limit {
		return entroq.InvalidArgumentf("%s is %d bytes, limit is %d", what, len(s), limit)
	}
	return nil
}

// Claimant checks a claimant that will be stored on a task or doc.
func Claimant(claimant string) error {
	return check("claimant", claimant, MaxClaimantBytes)
}

// Claim checks a task claim (see ClaimQuery.Validate) and its claimant's
// length. The client checks the same, so a claim failing here bypassed it.
func Claim(cq *entroq.ClaimQuery) error {
	if err := cq.Validate(); err != nil {
		return err
	}
	return Claimant(cq.Claimant)
}

// DocClaim checks a doc claim (see DocClaim.Validate), its claimant's length,
// and its duration, which the client has already defaulted.
func DocClaim(cq *entroq.DocClaim) error {
	if err := cq.Validate(); err != nil {
		return err
	}
	if cq.Duration == 0 && cq.TaskToMatch == nil {
		return entroq.InvalidArgumentf("doc claim must give a duration or a task to match")
	}
	for _, s := range cq.Sets {
		if err := check("doc namespace", s.Namespace, MaxNamespaceBytes); err != nil {
			return err
		}
		if err := check("doc key", s.Key, MaxDocKeyBytes); err != nil {
			return err
		}
	}
	return Claimant(cq.Claimant)
}

func doc(ns, id, key, secondaryKey string) error {
	if err := check("doc namespace", ns, MaxNamespaceBytes); err != nil {
		return err
	}
	if err := check("doc id", id, MaxIDBytes); err != nil {
		return err
	}
	if err := check("doc key", key, MaxDocKeyBytes); err != nil {
		return err
	}
	return check("doc secondary key", secondaryKey, MaxDocKeyBytes)
}

// Modification checks mod before a backend applies it: every write names its
// queue or namespace, no task or doc appears in more than one operation, and
// every value it would store is within its byte limit.
func Modification(mod *entroq.Modification) error {
	if err := mod.EnsureModifyKeys(); err != nil {
		return err
	}
	if err := docRefs(mod); err != nil {
		return err
	}
	if err := docPlaces(mod); err != nil {
		return err
	}
	if _, _, err := mod.AllDependencies(); err != nil {
		return err
	}
	return lengths(mod)
}

// docRefs checks that every doc reference names ONE thing. A DocID carries an ID
// for a doc and a key for a whole set, and the wire makes them exclusive (a
// protobuf oneof), so only a reference built in Go can carry both. Refusing it
// here means nothing downstream has to decide which one wins.
//
// A delete must name a doc. Deleting a whole set by key is a different
// operation, and silently deleting its single named member instead would be
// worse than refusing.
func docRefs(mod *entroq.Modification) error {
	both := func(what string, r *entroq.DocID) error {
		if r.ID != "" && r.Key != "" {
			return entroq.InvalidArgumentf("%s names both doc %q and set %q in namespace %q; name one",
				what, r.ID, r.Key, r.Namespace)
		}
		return nil
	}
	for _, d := range mod.DocDeletes {
		if err := both("doc delete", d); err != nil {
			return err
		}
		if d.IsSetRef() {
			return entroq.InvalidArgumentf("doc delete names set %q in namespace %q; a delete names a doc",
				d.Key, d.Namespace)
		}
	}
	for _, d := range mod.DocDepends {
		if err := both("doc depend", d); err != nil {
			return err
		}
	}
	return nil
}

// docPlaces checks that no two inserts take the same place in a set. A set is a
// map from secondary key to doc, so two inserts naming one secondary key in one
// set cannot both be right.
//
// This is a caller mistake rather than a condition of the stored world, so it
// reports as an invalid argument, the way an ID appearing in two operations does
// (see Modification.AllDependencies). The stored case -- a place another doc
// already holds -- is a collision and belongs to docset.Evaluate, which can see
// what is there. Neither check can do the other's job: two inserts in one
// modification are not stored yet, so no lookup finds them.
func docPlaces(mod *entroq.Modification) error {
	type place struct{ ns, key, secondary string }
	seen := make(map[place]bool, len(mod.DocInserts))
	for _, d := range mod.DocInserts {
		p := place{d.Namespace, d.Key, d.SecondaryKey}
		if seen[p] {
			return entroq.InvalidArgumentf(
				"two inserts name secondary key %q in set %q of namespace %q; a set holds one doc per secondary key",
				d.SecondaryKey, d.Key, d.Namespace)
		}
		seen[p] = true
	}
	return nil
}

// lengths checks every value mod would store. Deletes and depends only
// reference existing rows, so they are not checked, and the claimant is
// checked only when mod writes a row that records it.
func lengths(mod *entroq.Modification) error {
	writes := len(mod.Inserts) + len(mod.Changes) + len(mod.DocInserts) + len(mod.DocChanges) +
		len(mod.Arrives) + len(mod.DocArrives)
	if writes > 0 {
		if err := Claimant(mod.Claimant); err != nil {
			return err
		}
	}
	for _, t := range mod.Inserts {
		if err := check("task id", t.ID, MaxIDBytes); err != nil {
			return err
		}
	}
	for _, t := range mod.Changes {
		if err := check("task id", t.ID, MaxIDBytes); err != nil {
			return err
		}
	}
	for _, d := range mod.DocInserts {
		if err := doc(d.Namespace, d.ID, d.Key, d.SecondaryKey); err != nil {
			return err
		}
	}
	for _, d := range mod.DocChanges {
		if err := doc(d.Namespace, d.ID, d.Key, d.SecondaryKey); err != nil {
			return err
		}
	}
	return nil
}

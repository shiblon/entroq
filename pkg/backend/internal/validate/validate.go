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

// DocClaim checks the claimant a doc claim would store.
func DocClaim(cq *entroq.DocClaim) error {
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
	if _, _, err := mod.AllDependencies(); err != nil {
		return err
	}
	return lengths(mod)
}

// lengths checks every value mod would store. Deletes and depends only
// reference existing rows, so they are not checked, and the claimant is
// checked only when mod writes a row that records it.
func lengths(mod *entroq.Modification) error {
	writes := len(mod.Inserts) + len(mod.Changes) + len(mod.DocInserts) + len(mod.DocChanges)
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

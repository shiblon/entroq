package eqmem

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
)

// journalEntry is one journal record: a committed modification and the doc
// group locks it leaves. Claims and lock collection record locks alone.
// Entries written before doc groups had locks carry no DocLocks and parse
// unchanged.
type journalEntry struct {
	*entroq.Modification
	DocLocks []journalLock `json:"doc_locks,omitempty"`
}

// journalLock is a doc group's lock as journaled or snapshotted. Deleted
// records that the lock was collected.
type journalLock struct {
	Namespace string    `json:"namespace"`
	Key       string    `json:"key"`
	Version   int32     `json:"version"`
	Claimant  string    `json:"claimant,omitempty"`
	At        time.Time `json:"at"`
	Deleted   bool      `json:"deleted,omitempty"`
}

func (l journalLock) lock() docgroup.Lock {
	return docgroup.Lock{Version: l.Version, Claimant: l.Claimant, At: l.At}
}

func newJournalLock(g docgroup.Group, l docgroup.Lock) journalLock {
	return journalLock{Namespace: g.Namespace, Key: g.Key, Version: l.Version, Claimant: l.Claimant, At: l.At}
}

// journalLocksOf lists locks in a stable order, so a journal records the same
// entry for the same change.
func journalLocksOf(locks map[docgroup.Group]docgroup.Lock) []journalLock {
	jls := make([]journalLock, 0, len(locks))
	for g, l := range locks {
		jls = append(jls, newJournalLock(g, l))
	}
	slices.SortFunc(jls, func(a, b journalLock) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Key, b.Key))
	})
	return jls
}

// appendJournal writes entry to the journal, if there is one.
func (m *EQMem) appendJournal(entry journalEntry) error {
	if m.journal == nil {
		return nil
	}
	if entry.Modification == nil {
		entry.Modification = new(entroq.Modification)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal journal entry: %w", err)
	}
	if err := m.journal.Append(b); err != nil {
		return fmt.Errorf("append journal entry: %w", err)
	}
	return nil
}

// journalLocks records lock changes made without a modification.
func (m *EQMem) journalLocks(locks map[docgroup.Group]docgroup.Lock) error {
	return m.appendJournal(journalEntry{DocLocks: journalLocksOf(locks)})
}

// restoreLocks applies journaled or snapshotted locks. It runs only while the
// journal is loading, before any client can reach the backend.
func (m *EQMem) restoreLocks(jls []journalLock) {
	for _, jl := range jls {
		nls, unlock := m.lockNamespaces([]string{jl.Namespace})
		if jl.Deleted {
			nls[0].docs.DeleteLock(jl.Key)
		} else {
			nls[0].docs.SetLock(jl.Key, jl.lock())
		}
		unlock()
	}
}

// playJournalEntry replays one journal entry: its modification, then the locks
// it left.
func (m *EQMem) playJournalEntry(ctx context.Context, b []byte) error {
	var entry journalEntry
	if err := json.Unmarshal(b, &entry); err != nil {
		return fmt.Errorf("eqmem play journal entry: %w", err)
	}
	mod := entry.Modification
	if mod == nil {
		mod = new(entroq.Modification)
	}

	// Since changes represent the *final state* in the journal, we decrement
	// the version number before attempting to apply the modification so the
	// version-check in DependencyError passes.
	for _, chg := range mod.Changes {
		chg.Version--
	}
	for _, chg := range mod.DocChanges {
		chg.Version--
	}

	// Entries without locks were written before doc groups had them, when a
	// doc claim was not journaled, so their doc versions can trail what the
	// modification names. Replay applies them anyway; count them for one
	// warning at the end.
	if entry.DocLocks == nil {
		m.countStaleDocReplays(mod)
	}

	if _, err := m.modifyImpl(ctx, mod, true); err != nil {
		return fmt.Errorf("eqmem play journal entry: %w", err)
	}
	m.restoreLocks(entry.DocLocks)
	return nil
}

// countStaleDocReplays counts the doc operations in mod that name a version
// other than the stored doc's.
func (m *EQMem) countStaleDocReplays(mod *entroq.Modification) {
	check := func(ns, id string, version int32) {
		nls, unlock := m.lockNamespaces([]string{ns})
		defer unlock()
		if d, ok := nls[0].docs.Get(id); !ok || d.Version != version {
			m.staleDocReplays++
			if len(m.staleDocExamples) < 3 {
				m.staleDocExamples = append(m.staleDocExamples, entroq.NewDocID(ns, id, version).String())
			}
		}
	}
	for _, c := range mod.DocChanges {
		check(c.Namespace, c.ID, c.Version)
	}
	for _, d := range mod.DocDeletes {
		check(d.Namespace, d.ID, d.Version)
	}
	for _, d := range mod.DocDepends {
		check(d.Namespace, d.ID, d.Version)
	}
}

// finishDocReplay runs once the journal has loaded. It reports stale doc
// versions replay applied, and gives every group written before groups had
// locks a lock one version past its highest member, so any version read
// before then is stale.
func (m *EQMem) finishDocReplay() {
	if m.staleDocReplays > 0 {
		log.Printf("eqmem journal replay: %d doc operations named a version other than the stored doc's (for example %v), "+
			"from doc claims journals did not record before v1.13; the recorded state was applied. "+
			"Take a snapshot with this version (eqmem serve --snapshot_and_quit) before upgrading past 1.13, "+
			"which checks doc versions on replay again.",
			m.staleDocReplays, m.staleDocExamples)
	}
	for _, ns := range m.namespaces {
		highest := make(map[string]int32)
		for _, d := range ns.byID {
			if v, ok := highest[d.Key]; !ok || d.Version > v {
				highest[d.Key] = d.Version
			}
		}
		for key, v := range highest {
			if ns.Lock(key) == docgroup.Absent {
				ns.SetLock(key, docgroup.Lock{Version: v + 1})
			}
		}
	}
}

// collectLocksOnce removes the locks of up to batch groups that have no docs
// and are not held, so claimed-then-abandoned groups do not accumulate. Each
// namespace is checked under its own lock, so a concurrent insert either comes
// first, leaving the group non-empty, or after, starting a new lock.
func (m *EQMem) collectLocksOnce(ctx context.Context, batch int) (int, error) {
	now, err := m.Time(ctx)
	if err != nil {
		return 0, fmt.Errorf("eqmem lock gc: %w", err)
	}
	var names []string
	func() {
		defer un(lock(m))
		for name := range m.namespaces {
			names = append(names, name)
		}
	}()

	collected := make(map[docgroup.Group]docgroup.Lock)
	for _, name := range names {
		if len(collected) >= batch || ctx.Err() != nil {
			break
		}
		func() {
			nls, unlock := m.lockNamespaces([]string{name})
			defer unlock()
			nss := nls[0].docs
			var empty []lockEntry
			nss.locks.Ascend(func(e lockEntry) bool {
				if len(collected)+len(empty) >= batch {
					return false
				}
				if !e.Lock.Held(now) && len(nss.Members(e.Key)) == 0 {
					empty = append(empty, e)
				}
				return true
			})
			for _, e := range empty {
				nss.DeleteLock(e.Key)
				collected[docgroup.Group{Namespace: name, Key: e.Key}] = e.Lock
			}
		}()
	}
	if len(collected) == 0 {
		return 0, nil
	}

	jls := journalLocksOf(collected)
	for i := range jls {
		jls[i].Deleted = true
	}
	if err := m.appendJournal(journalEntry{DocLocks: jls}); err != nil {
		log.Fatalf("Inconsistent internal state: doc locks collected but could not be journaled: %v", err)
	}
	return len(collected), nil
}

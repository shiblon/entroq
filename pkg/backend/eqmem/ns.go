package eqmem

import (
	"context"
	"fmt"

	"github.com/shiblon/entroq"
)

// NamespaceStats returns doc counts per namespace, optionally filtered and limited.
func (m *EQMem) NamespaceStats(ctx context.Context, qq *entroq.MatchQuery) (map[string]*entroq.NamespaceStat, error) {
	now, err := m.Time(ctx)
	if err != nil {
		return nil, fmt.Errorf("namespace stats time: %w", err)
	}

	// Snapshot namespace lock pointers under the global lock.
	type nsEntry struct {
		name string
		nl   *nsLock
	}
	var entries []nsEntry
	func() {
		defer un(lock(m))
		for name, nl := range m.locksSuperUnsafeNS {
			entries = append(entries, nsEntry{name, nl})
		}
	}()

	result := make(map[string]*entroq.NamespaceStat)
	for _, e := range entries {
		if !matchesQuery(e.name, qq) {
			continue
		}
		if qq.Limit > 0 && len(result) >= qq.Limit {
			break
		}
		// Clone under the namespace lock, then iterate the snapshot lock-free.
		e.nl.Lock()
		snap := e.nl.docs.snapshot()
		e.nl.Unlock()

		stat := &entroq.NamespaceStat{Name: e.name}
		snap.Ascend(func(entry docKeyEntry) bool {
			d := entry.Doc
			stat.Size++
			if d.At.After(now) && d.Claimant != "" {
				stat.Claimed++
			}
			return true
		})
		result[e.name] = stat
	}
	return result, nil
}

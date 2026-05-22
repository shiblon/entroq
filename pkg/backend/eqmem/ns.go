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

	// Snapshot namespace pointers under the global lock; iterate without locks.
	// sync.Map.Range is safe concurrently; eventual consistency is fine for stats.
	type nsEntry struct {
		name string
		ns   *docNamespace
	}
	var entries []nsEntry
	func() {
		defer un(lock(m))
		for name, ns := range m.namespaces {
			entries = append(entries, nsEntry{name, ns})
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
		stat := &entroq.NamespaceStat{Name: e.name}
		e.ns.Range(func(_ string, d *entroq.Doc) bool {
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

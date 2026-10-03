// Package arrival turns a modification's task arrivals into ordinary changes,
// so a backend's Modify checks and writes them as it does any change.
package arrival

import (
	"slices"

	"github.com/shiblon/entroq"
)

// Changes returns mod with its task arrivals appended to its changes, and none
// left as arrivals. Each becomes the change TaskArrival.Change makes from its
// stored task, which the change path then holds or releases as for any change.
// A backend calls it once the named tasks are loaded.
func Changes(mod *entroq.Modification, stored func(id string) *entroq.Task) *entroq.Modification {
	if len(mod.Arrives) == 0 {
		return mod
	}
	m := *mod
	m.Changes = slices.Clone(mod.Changes)
	m.Arrives = nil
	for _, a := range mod.Arrives {
		m.Changes = append(m.Changes, a.Change(stored(a.ID)))
	}
	return &m
}

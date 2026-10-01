// Package arrival turns a modification's task arrivals into ordinary changes,
// so a backend's Modify checks and writes them as it does any change.
package arrival

import (
	"slices"
	"time"

	"github.com/shiblon/entroq"
)

// Changes returns mod with its task arrivals appended to its changes, and
// none left as arrivals. Each becomes its stored task, from stored, with only
// its arrival time moved, to the arrival's At, which the change path then
// holds or releases as for any change, keeping its claim count, as a change
// does unless it resets it. It keeps the version and queue the arrival named, so Modify's version, queue,
// and claim checks apply as for any change, and a missing task becomes a
// change that fails as missing. A backend calls it once the named tasks are
// loaded, with the time it checks against.
func Changes(mod *entroq.Modification, now time.Time, stored func(id string) *entroq.Task) *entroq.Modification {
	if len(mod.Arrives) == 0 {
		return mod
	}
	m := *mod
	m.Changes = slices.Clone(mod.Changes)
	m.Arrives = nil
	for _, a := range mod.Arrives {
		c := &entroq.Task{ID: a.ID}
		if t := stored(a.ID); t != nil {
			c = t.Copy()
		}
		c.Version, c.Queue, c.FromQueue, c.At = a.Version, a.Queue, a.Queue, entroq.NormalizeArrival(a.At, now)
		m.Changes = append(m.Changes, c)
	}
	return &m
}

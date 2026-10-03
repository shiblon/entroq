package entroq

import (
	"context"
	"time"
)

// Ref names the set at its version.
func (g *DocSet) Ref() *DocID {
	return NewDocSetRef(g.Namespace, g.Key, g.Version)
}

// TaskArrival changes only when a task is ready again: at At, holding it until
// then, or now, releasing it, for an At that has already passed. The task
// moves one version, and keeps its value, queue, attempts, and claim count.
type TaskArrival struct {
	TaskID
	At time.Time `json:"at"`
}

// DocArrival changes only when a doc set is ready again, as TaskArrival does
// for a task. It names the set by reference (see DocID.IsSetRef). The set may
// have no docs.
type DocArrival struct {
	DocID
	At time.Time `json:"at"`
}

// ArrivalEntry makes tasks and doc sets ready again after a duration. Build
// one with ReadyIn or ReadyNow, name what it covers with Tasks and Docs, and
// apply it with Arriving or UpdateArrival:
//
//	eq.UpdateArrival(ctx,
//		entroq.ReadyIn(lease).Tasks(task).Docs(working...),
//		entroq.ReadyNow().Docs(finished...),
//	)
type ArrivalEntry struct {
	in    time.Duration
	at    time.Time
	tasks []*Task
	sets  []*DocSet
}

// ReadyIn returns an entry making its items ready again d from when the
// modification is made: a renewal of their claim for d.
func ReadyIn(d time.Duration) *ArrivalEntry {
	return &ArrivalEntry{in: d}
}

// ReadyAt returns an entry making its items ready again at t.
func ReadyAt(t time.Time) *ArrivalEntry {
	return &ArrivalEntry{at: t}
}

// ReadyNow returns an entry making its items ready now, releasing them.
func ReadyNow() *ArrivalEntry {
	return new(ArrivalEntry)
}

// When returns when the entry's items are ready again, for a modification
// made at now. The zero time means now, and is deliberately left zero for the
// backend to resolve against its own clock: a release must happen at the
// backend's now, not at this process's, or a client whose clock runs fast
// would release an item into the future and leave it unavailable.
func (e *ArrivalEntry) When(now time.Time) time.Time {
	if e.in > 0 {
		return now.Add(e.in)
	}
	return e.at
}

// Tasks adds tasks to the entry, each at the version given.
func (e *ArrivalEntry) Tasks(tasks ...*Task) *ArrivalEntry {
	e.tasks = append(e.tasks, tasks...)
	return e
}

// Docs adds doc sets to the entry, each at the version given. Only a
// set's name and version matter here.
func (e *ArrivalEntry) Docs(sets ...*DocSet) *ArrivalEntry {
	e.sets = append(e.sets, sets...)
	return e
}

// Arriving adds the entries' arrival changes to a modification. Its tasks come
// back among the response's changed tasks, and its doc sets among its
// changed sets.
func Arriving(entries ...*ArrivalEntry) ModifyArg {
	return func(m *Modification) {
		now := ProcessTime()
		for _, e := range entries {
			at := e.When(now)
			for _, t := range e.tasks {
				m.Arrives = append(m.Arrives, &TaskArrival{TaskID: *t.IDVersion(), At: at})
			}
			for _, g := range e.sets {
				m.DocArrives = append(m.DocArrives, &DocArrival{DocID: *g.Ref(), At: at})
			}
		}
	}
}

// UpdateArrival changes when tasks and doc sets this client holds are ready
// again, renewing or releasing them, in one atomic modification. It is Modify
// with Arriving: every item must be at the version named and not held by
// anyone else, or nothing changes, and each moves one version and nothing
// else.
func (c *EntroQ) UpdateArrival(ctx context.Context, entries ...*ArrivalEntry) (*ModifyResponse, error) {
	var mod Modification
	Arriving(entries...)(&mod)
	if len(mod.Arrives) == 0 && len(mod.DocArrives) == 0 {
		return nil, InvalidArgumentf("update arrival: no tasks or doc sets")
	}
	return c.Modify(ctx, Arriving(entries...))
}

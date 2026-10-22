package entroq

import (
	"context"
	"time"
)

// Ref names the set at its version.
func (g *DocSet) Ref() *DocID {
	return NewDocSetRef(g.Namespace, g.Key, g.Version)
}

// TaskArrival changes only when a task is ready again: By from the backend's
// own now, holding it until then, or now, releasing it, for a By of zero or
// less. The task moves one version, and keeps its value, queue, attempts, and
// claim count.
type TaskArrival struct {
	TaskID
	By time.Duration `json:"by"`
}

// Change returns the ordinary change that makes this arrival, from the task as
// stored, with only its arrival moved, so a backend's Modify checks and writes
// it as it does any change. It keeps the version and queue the arrival named,
// so the version, queue, and claim checks apply as for any change, and keeps
// the claim count, as a change does unless it resets it. A nil stored task
// gives a change that fails as missing. The stored instant is left behind: a
// change names its arrival only as a duration, and carrying the instant along
// would let a backend that still read it hold the old arrival silently.
func (a *TaskArrival) Change(stored *Task) *Task {
	c := &Task{ID: a.ID}
	if stored != nil {
		c = stored.Copy()
	}
	c.Version, c.Queue, c.FromQueue = a.Version, a.Queue, a.Queue
	c.At, c.by = time.Time{}, a.By
	return c
}

// DocArrival changes only when a doc set is ready again, as TaskArrival does
// for a task: By from the backend's own now, holding it until then, or now,
// releasing it, for a By of zero or less. It names the set by reference (see
// DocID.IsSetRef). The set may have no docs.
type DocArrival struct {
	DocID
	By time.Duration `json:"by"`
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
	// rel says in is the arrival, even at zero or less, so a duration that
	// came off the wire as a release or an already-passed arrival is not read
	// as "no duration given" and quietly replaced by at.
	rel   bool
	in    time.Duration
	at    time.Time
	tasks []*Task
	sets  []*DocSet
}

// ReadyIn returns an entry making its items ready again d from when the
// modification is made: a renewal of their claim for d. A d of zero or less
// makes them ready now, releasing them.
func ReadyIn(d time.Duration) *ArrivalEntry {
	return &ArrivalEntry{rel: true, in: d}
}

// ReadyAt returns an entry making its items ready again at t.
func ReadyAt(t time.Time) *ArrivalEntry {
	return &ArrivalEntry{at: t}
}

// ReadyNow returns an entry making its items ready now, releasing them.
func ReadyNow() *ArrivalEntry {
	return new(ArrivalEntry)
}

// By returns how long after a modification made at now the entry's items are
// ready again. Zero means now, releasing them. An entry built from an instant
// is converted here, at the edge, because an arrival travels as a duration:
// the offset between this process's clock and the backend's cancels out of a
// duration, and does not out of an instant.
func (e *ArrivalEntry) By(now time.Time) time.Duration {
	switch {
	case e.rel:
		return e.in
	case e.at.IsZero():
		return 0
	default:
		return e.at.Sub(now)
	}
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
			by := e.By(now)
			for _, t := range e.tasks {
				m.Arrives = append(m.Arrives, &TaskArrival{TaskID: *t.IDVersion(), By: by})
			}
			for _, g := range e.sets {
				m.DocArrives = append(m.DocArrives, &DocArrival{DocID: *g.Ref(), By: by})
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

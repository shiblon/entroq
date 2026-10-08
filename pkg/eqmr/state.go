package eqmr

import (
	"context"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
)

// Run phases, recorded in the state doc.
const (
	PhaseMap    = "map"
	PhaseReduce = "reduce"
	PhaseDone   = "done"
	PhaseFailed = "failed"
)

// stateDocKey and stateDocID name the run's single state doc. The explicit id
// makes a second Setup against the same prefix collide instead of starting a
// second run over the first.
const (
	stateDocKey = "state"
	stateDocID  = "state"
)

// runState is the content of the state doc: the run's phase and the facts
// fixed when it was set up. Progress within a phase is not kept here. It is
// derived from the docs the phase consumes and produces, so accepting one unit
// of work never touches the run as a whole; only the checks that move the run
// between phases claim and change this doc.
type runState struct {
	Phase string `json:"phase"`
	// Reason explains a PhaseFailed outcome.
	Reason string `json:"reason,omitempty"`

	// MapSplits is the number of split docs Setup wrote.
	MapSplits int `json:"map_splits"`
	// ReduceParts is the partition count the run was created with.
	ReduceParts int `json:"reduce_parts"`

	// Store holds the run's intermediate data, and Sink receives its output,
	// both recorded by Setup.
	Store Descriptor `json:"store"`
	Sink  Descriptor `json:"sink"`
}

func (s runState) terminal() bool {
	return s.Phase == PhaseDone || s.Phase == PhaseFailed
}

func (s runState) failed(reason string) runState {
	s.Phase = PhaseFailed
	s.Reason = reason
	return s
}

// Status reports the run's current phase, and the failure reason when the run
// has failed. It reads the state doc, so it works from any process.
func (c *Controller) Status(ctx context.Context) (phase, reason string, err error) {
	doc, st, err := c.stateDoc(ctx)
	if err != nil {
		return "", "", fmt.Errorf("eqmr status: %w", err)
	}
	if doc == nil {
		return "", "", fmt.Errorf("eqmr status: no state doc in %s (run not set up)", c.DocNS())
	}
	return st.Phase, st.Reason, nil
}

// maxWaitInterval caps how long Wait sleeps between checks. Wait is a
// client-side observer of durable state, so its latency is a separate question
// from anything the controller does. Status is one cheap doc read, so a brisk fixed
// cap costs little.
const maxWaitInterval = 100 * time.Millisecond

// Wait blocks until the run reaches a terminal phase, returning an error if it
// failed. Call it after Setup.
//
// It polls, because a run's progress lives in EntroQ rather than in this
// process. It is a convenience for in-process runs; a long-lived deployment
// should call Status on whatever schedule suits it rather than blocking here.
func (c *Controller) Wait(ctx context.Context) error {
	ticker := time.NewTicker(maxWaitInterval)
	defer ticker.Stop()
	for {
		phase, reason, err := c.Status(ctx)
		if err != nil {
			return err
		}
		switch phase {
		case PhaseDone:
			return nil
		case PhaseFailed:
			return fmt.Errorf("eqmr run %q failed: %s", c.prefix, reason)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("eqmr wait: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// CellState is the state of one unit of work: one input split in the map phase,
// or one partition in the reduce phase.
type CellState uint8

const (
	// CellPending means the unit exists and nobody is working on it.
	CellPending CellState = iota
	// CellRunning means a worker holds it right now.
	CellRunning
	// CellDone means it committed its output.
	CellDone
)

// String renders a cell state for logs and simple text rendering.
func (s CellState) String() string {
	switch s {
	case CellRunning:
		return "running"
	case CellDone:
		return "done"
	default:
		return "pending"
	}
}

// PhaseProgress counts the units of one phase. Total is fixed for the life of
// the run; the three states below always sum to it.
type PhaseProgress struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Running int `json:"running"`
	Pending int `json:"pending"`
}

// Progress is a snapshot of a run, sufficient to draw it.
//
// Counts are per phase, not per worker. A unit is claimed by whichever worker
// is free, so no worker owns a knowable share of the run. For per-worker
// throughput and idleness use the worker metrics, entroq.worker.tasks_total and
// entroq.worker.slots.
//
// Map resolution follows the split count: a mapper commits once, at the end of
// its split, so finer progress comes from smaller splits.
type Progress struct {
	Phase  string        `json:"phase"`
	Reason string        `json:"reason,omitempty"`
	Map    PhaseProgress `json:"map"`
	Reduce PhaseProgress `json:"reduce"`
	// Quarantined is the number of tasks waiting in or judged by the error
	// path. Any value above zero means the run is failing or has failed,
	// unless only collection failed.
	Quarantined int `json:"quarantined"`
}

// Progress reports a snapshot of the run. It is safe to call from any process
// and at any time, including after completion.
//
// Counts are derived from the docs each phase consumes, best effort: a split
// doc is deleted when its map finish is accepted, and a partition's pointers
// when its reduce finish is. A partition that received no data has nothing to
// reduce and counts as done once the map phase ends.
func (c *Controller) Progress(ctx context.Context) (*Progress, error) {
	doc, st, err := c.stateDoc(ctx)
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("eqmr progress: no state doc in %s (run not set up)", c.DocNS())
	}
	p := &Progress{Phase: st.Phase, Reason: st.Reason}

	// "Running" is a count of claimed TASKS, not claimed documents. Workers do
	// not claim their input, so that a duplicate task can race a straggler
	// rather than block on it; the task claim is what remains observable.
	// Duplicates inflate the raw claimed count, since two workers may hold two
	// tasks for one unit of work, so it is capped by the units outstanding.
	queues := append([]string{c.MapQ(), c.ReduceQ(), c.QuarantineQ()}, c.errQueues()...)
	stats, err := c.client.QueueStats(ctx, entroq.MatchExact(queues...))
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: queue stats: %w", err)
	}

	mapLeft, err := c.countDocs(ctx, splitPrefix)
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: %w", err)
	}
	mapLeft = min(mapLeft, st.MapSplits)
	mapRunning := min(queueClaimed(stats, c.MapQ()), mapLeft)
	p.Map = PhaseProgress{
		Total:   st.MapSplits,
		Done:    st.MapSplits - mapLeft,
		Running: mapRunning,
		Pending: mapLeft - mapRunning,
	}

	reduceLeft := st.ReduceParts
	if st.Phase != PhaseMap {
		pending, err := c.keyPartitions(ctx, mapOutPrefix)
		if err != nil {
			return nil, fmt.Errorf("eqmr progress: %w", err)
		}
		reduceLeft = min(len(pending), st.ReduceParts)
	}
	reduceRunning := min(queueClaimed(stats, c.ReduceQ()), reduceLeft)
	p.Reduce = PhaseProgress{
		Total:   st.ReduceParts,
		Done:    st.ReduceParts - reduceLeft,
		Running: reduceRunning,
		Pending: reduceLeft - reduceRunning,
	}

	p.Quarantined = queueDepth(stats, c.QuarantineQ())
	for _, q := range c.errQueues() {
		p.Quarantined += queueDepth(stats, q)
	}
	return p, nil
}

// countDocs counts the docs in the run's namespace with keys under prefix,
// listing keys only.
func (c *Controller) countDocs(ctx context.Context, prefix string) (int, error) {
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{
		Namespace:  c.DocNS(),
		KeyStart:   prefix,
		KeyEnd:     prefixEnd(prefix),
		OmitValues: true,
	})
	if err != nil {
		return 0, fmt.Errorf("count %q docs: %w", prefix, err)
	}
	return len(docs), nil
}

func queueDepth(stats map[string]*entroq.QueueStat, q string) int {
	s, ok := stats[q]
	if !ok {
		return 0
	}
	return s.Size
}

func queueClaimed(stats map[string]*entroq.QueueStat, q string) int {
	s, ok := stats[q]
	if !ok {
		return 0
	}
	return s.Claimed
}

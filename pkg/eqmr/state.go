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

// stateDocKey and stateDocID name the run's state doc. The explicit id makes a
// second run on the same prefix collide.
const (
	stateDocKey = "state"
	stateDocID  = "state"
)

// runState is the content of the state doc: the run's phase and what Setup
// fixed. Only phase transitions change it; progress within a phase is derived
// from the docs.
type runState struct {
	Phase string `json:"phase"`
	// Reason explains a PhaseFailed outcome.
	Reason string `json:"reason,omitempty"`

	// MapSplits is the number of split docs Setup wrote.
	MapSplits int `json:"map_splits"`
	// ReduceParts is the partition count the run was created with.
	ReduceParts int `json:"reduce_parts"`

	// Store holds the run's intermediate data, and Sink receives its output.
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

// Status reports the run's phase, and the reason if it failed. It works from
// any process.
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

// maxWaitInterval is how often Wait checks Status, which is one doc read.
const maxWaitInterval = 100 * time.Millisecond

// Wait polls until the run is done or failed, returning an error if it failed.
// Call it after Setup. A long-lived deployment can call Status on its own
// schedule instead.
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

// Progress is a snapshot of a run, enough to draw it. Counts are per phase;
// per-worker throughput is in the worker metrics, entroq.worker.tasks_total and
// entroq.worker.slots. A mapper commits once per split, so finer progress comes
// from smaller splits.
type Progress struct {
	Phase  string        `json:"phase"`
	Reason string        `json:"reason,omitempty"`
	Map    PhaseProgress `json:"map"`
	Reduce PhaseProgress `json:"reduce"`
	// Quarantined counts tasks in the error queues and the quarantine queue.
	// Above zero, the run has failed or is failing, unless only collection
	// failed.
	Quarantined int `json:"quarantined"`
}

// Progress reports a snapshot of the run, from any process and at any time.
// Counts are derived from the docs each phase consumes, best effort. A
// partition with no data counts as done once the map phase ends.
func (c *Controller) Progress(ctx context.Context) (*Progress, error) {
	doc, st, err := c.stateDoc(ctx)
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("eqmr progress: no state doc in %s (run not set up)", c.DocNS())
	}
	p := &Progress{Phase: st.Phase, Reason: st.Reason}

	// Running counts claimed tasks, capped by the units outstanding, since
	// duplicate tasks for one unit are claimed separately.
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

// countDocs counts the docs with keys under prefix.
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

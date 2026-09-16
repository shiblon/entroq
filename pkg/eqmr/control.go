package eqmr

import (
	"context"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// ControlWorker returns the worker that drives phase transitions.
//
// It watches ControlQ, which holds exactly one task for the life of a run. That
// task carries the phase state: each tick claims it, checks the current phase
// barrier, and commits the next state, plus anything that state change fans
// out, in a single atomic Modify. EntroQ's claim semantics mean only one
// controller can act at a time no matter how many run, and a controller that
// dies mid-tick simply loses its claim, so another picks the task up once the
// claim is released.
//
// Run as many control pods as you like; exactly one will be doing anything.
func (c *Controller) ControlWorker() *worker.Worker[controlState] {
	return worker.New[controlState](c.client,
		worker.WithErrQMap[controlState](c.errQMap),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, state controlState, _ []*entroq.Doc) (*worker.Result, error) {
			// Check the barrier FIRST, and pace only when it did not move.
			// Sleeping before looking would add a full ControlInterval to every
			// phase, since a phase that is already complete would still wait out
			// an interval before anyone noticed.
			next, extra, err := c.step(ctx, state)
			if err != nil {
				return nil, err
			}

			// Terminal states park far out so a finished run stops costing
			// claims while staying inspectable. A phase that just advanced
			// commits immediately and is re-examined without waiting, because
			// the next barrier is a different question and may already be
			// answered. Only a phase that is still waiting on the same barrier
			// pauses, and it does so here, under renewal.
			//
			// Pacing with a sleep rather than a near-future arrival time is
			// deliberate: an arrival time expresses "not before this moment",
			// not "wake me again shortly". Holding the claim across the sleep
			// costs nothing and leaves the single-actor guarantee unchanged.
			delay := time.Duration(0)
			switch {
			case next.Phase == PhaseDone || next.Phase == PhaseFailed:
				delay = terminalRearm
			case next.Phase == state.Phase:
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(c.cfg.ControlInterval):
				}
			}
			// The phase change and whatever it fans out commit together, so a
			// controller that dies between them is not a possible state.
			modArgs := append([]entroq.ModifyArg{task.Change(
				entroq.ValueTo(next),
				entroq.ArrivalTimeBy(delay),
			)}, extra...)
			return worker.Modify(modArgs...), nil
		}),
	)
}

// step computes the next control state, plus any modifications that must land
// atomically with it. It performs no mutation itself: everything it decides is
// committed by the caller in a single Modify alongside the control task.
func (c *Controller) step(ctx context.Context, state controlState) (controlState, []entroq.ModifyArg, error) {
	// Terminal states re-arm without doing work, so the outcome stays readable
	// with ordinary task tooling until Cleanup runs.
	if state.Phase == PhaseDone || state.Phase == PhaseFailed {
		return state, nil, nil
	}

	// A quarantined task fails the run: a MapReduce that lost part of its input
	// cannot support a claim about the correctness of its output.
	stats, err := c.client.QueueStats(ctx, entroq.MatchExact(c.ErrQ()))
	if err != nil {
		return state, nil, fmt.Errorf("eqmr control: quarantine check: %w", err)
	}
	if n := queueDepth(stats, c.ErrQ()); n > 0 {
		return failed(state, c.quarantineReason(ctx, n)), nil, nil
	}

	switch state.Phase {
	case PhaseMap:
		// Split docs are deleted in the same Modify that publishes pointers to
		// already-durable runs, so their absence means every map output is reachable.
		remaining, err := c.countDocs(ctx, splitPrefix)
		if err != nil {
			return state, nil, fmt.Errorf("eqmr control: %w", err)
		}
		if remaining == 0 {
			next := controlState{
				Phase:        PhaseReduce,
				MapSplits:    state.MapSplits,
				ReduceParts:  state.ReduceParts,
				LastProgress: time.Now(),
			}
			return next, c.pendingReduceTasks(state.ReduceParts), nil
		}
		return c.progress(state, remaining)

	case PhaseReduce:
		// Every partition writes a result doc, empty ones included, and each is
		// committed atomically with that partition's map output deletions. So the
		// count of result docs is a complete and purely doc-based statement of
		// how much of the reduce phase is finished. No queue is consulted.
		done, err := c.countDocs(ctx, resultPrefix)
		if err != nil {
			return state, nil, fmt.Errorf("eqmr control: %w", err)
		}
		if done >= state.ReduceParts {
			next := controlState{
				Phase:        PhaseDone,
				MapSplits:    state.MapSplits,
				ReduceParts:  state.ReduceParts,
				LastProgress: time.Now(),
			}
			return next, nil, nil
		}
		return c.progress(state, state.ReduceParts-done)

	default:
		return failed(state, fmt.Sprintf("unknown phase %q", state.Phase)), nil, nil
	}
}

// pendingReduceTasks is the modification that fans a run out to its reduce
// partitions. It is applied by the same Modify that advances the control task,
// so the fan-out and the phase change cannot diverge.
func (c *Controller) pendingReduceTasks(parts int) []entroq.ModifyArg {
	args := make([]entroq.ModifyArg, 0, parts)
	for p := range parts {
		args = append(args, entroq.InsertingInto(c.ReduceQ(), entroq.WithValue(reduceClaim{
			Doc:       docRef{NS: c.DocNS(), Key: mapOutDocKey(p)},
			Partition: p,
		})))
	}
	return args
}

// progress folds a stall check into the next state. A phase whose outstanding
// unit count has not moved for StallTimeout has stopped making progress, which
// otherwise presents as a run that hangs forever with no diagnostic.
//
// Claimed work counts as progress so that a single slow mapper is never mistaken
// for a stall: its unit is claimed and renewed the whole time it runs, and the
// remaining count does not move until it commits. A worker that hangs while
// still renewing its claim is out of scope here, as it must be: that is a
// process liveness problem for the orchestrator, not something the queue can
// distinguish from slow work.
func (c *Controller) progress(state controlState, remaining int) (controlState, []entroq.ModifyArg, error) {
	now := time.Now()
	next := state
	next.LastRemaining = remaining

	if remaining != state.LastRemaining || state.LastProgress.IsZero() {
		next.LastProgress = now
		return next, nil, nil
	}
	if c.cfg.StallTimeout > 0 && now.Sub(state.LastProgress) > c.cfg.StallTimeout {
		claimed, err := c.claimedTasks(context.Background())
		if err == nil && claimed > 0 {
			// Something is actively held and being renewed: slow, not stalled.
			next.LastProgress = now
			return next, nil, nil
		}
		return failed(state, fmt.Sprintf("phase %q made no progress for %v (%d unit(s) outstanding)",
			state.Phase, now.Sub(state.LastProgress).Round(time.Second), remaining)), nil, nil
	}
	return next, nil, nil
}

// quarantineReason describes a quarantine failure, including why the first
// quarantined task got there.
//
// A bare count says a run failed without saying what to change, and the reason
// is right there: the worker records it on the task when it quarantines. Only
// reached on the terminal failure path, so one extra listing costs nothing.
func (c *Controller) quarantineReason(ctx context.Context, n int) string {
	base := fmt.Sprintf("%d task(s) quarantined in %s", n, c.ErrQ())
	tasks, err := c.client.Tasks(ctx, c.ErrQ(), entroq.LimitTasks(1))
	if err != nil || len(tasks) == 0 || tasks[0].Err == "" {
		return base
	}
	return fmt.Sprintf("%s; first: %s", base, tasks[0].Err)
}

func failed(state controlState, reason string) controlState {
	state.Phase = PhaseFailed
	state.Reason = reason
	return state
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

// Status reports the run's current phase, and the failure reason when the run
// has failed. It reads the control task, so it works from any process.
func (c *Controller) Status(ctx context.Context) (phase, reason string, err error) {
	tasks, err := c.client.Tasks(ctx, c.ControlQ())
	if err != nil {
		return "", "", fmt.Errorf("eqmr status: %w", err)
	}
	if len(tasks) == 0 {
		return "", "", fmt.Errorf("eqmr status: no control task in %s (run not set up, or already cleaned up)", c.ControlQ())
	}
	state, err := entroq.GetValue[controlState](tasks[0])
	if err != nil {
		return "", "", fmt.Errorf("eqmr status: parse control task: %w", err)
	}
	return state.Phase, state.Reason, nil
}

// maxWaitInterval caps how long Wait sleeps between checks. Wait is a
// client-side observer of a durable state machine, so its latency is a separate
// question from the controller's cadence: polling on ControlInterval would make
// a finished run take up to a full interval longer to be noticed, purely as
// observation lag. Status is one cheap query against a single-task queue, so a
// brisk fixed cap costs little.
const maxWaitInterval = 100 * time.Millisecond

// Wait blocks until the run reaches a terminal phase, returning an error if it
// failed. Call it after Setup.
//
// It polls, because a run's progress lives in EntroQ rather than in this
// process. It is a convenience for in-process runs; a long-lived deployment
// should call Status on whatever schedule suits it rather than blocking here.
func (c *Controller) Wait(ctx context.Context) error {
	interval := min(c.cfg.ControlInterval, maxWaitInterval)
	ticker := time.NewTicker(interval)
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
	// Quarantined is the number of tasks that exhausted their retries. Any
	// value above zero means the run is failing or has failed.
	Quarantined int `json:"quarantined"`
}

// Progress reports a snapshot of the run. It is safe to call from any process
// and at any time, including after completion and before Cleanup.
//
// Completion is read purely from documents: a split doc disappears when its
// mapper commits, and a result doc appears when a partition commits, empty
// partitions included. Neither answer consults a queue, so a shared or
// long-lived queue would not change these numbers.
func (c *Controller) Progress(ctx context.Context) (*Progress, error) {
	tasks, err := c.client.Tasks(ctx, c.ControlQ())
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: %w", err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("eqmr progress: no control task in %s (run not set up, or already cleaned up)", c.ControlQ())
	}
	state, err := entroq.GetValue[controlState](tasks[0])
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: parse control task: %w", err)
	}

	p := &Progress{Phase: state.Phase, Reason: state.Reason}

	// One stats call covers both work queues and the quarantine queue.
	//
	// "Running" is a count of claimed TASKS, not claimed documents. Workers no
	// longer claim their input, so that a duplicate task can race a straggler
	// rather than block on it; the task claim is what remains observable. A
	// consequence is that duplicates inflate the raw claimed count, since two
	// workers may hold two tasks for one unit of work, so it is capped by the
	// number of units actually outstanding.
	stats, err := c.client.QueueStats(ctx, entroq.MatchExact(c.MapQ(), c.ReduceQ(), c.ErrQ()))
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: queue stats: %w", err)
	}

	// Map: outstanding splits are the ones whose docs are still present.
	remaining, err := c.countDocs(ctx, splitPrefix)
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: %w", err)
	}
	mapRunning := min(queueClaimed(stats, c.MapQ()), remaining)
	p.Map = PhaseProgress{
		Total:   state.MapSplits,
		Done:    max(state.MapSplits-remaining, 0),
		Running: mapRunning,
		Pending: max(remaining-mapRunning, 0),
	}

	// Reduce: a partition is done once its result doc exists.
	done, err := c.countDocs(ctx, resultPrefix)
	if err != nil {
		return nil, fmt.Errorf("eqmr progress: %w", err)
	}
	outstanding := max(state.ReduceParts-done, 0)
	reduceRunning := min(queueClaimed(stats, c.ReduceQ()), outstanding)
	p.Reduce = PhaseProgress{
		Total:   state.ReduceParts,
		Done:    min(done, state.ReduceParts),
		Running: reduceRunning,
		Pending: max(outstanding-reduceRunning, 0),
	}

	p.Quarantined = queueDepth(stats, c.ErrQ())
	return p, nil
}

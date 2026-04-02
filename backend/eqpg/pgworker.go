package eqpg

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
)

const DefaultLeaseTime = 5 * time.Minute
const DefaultBaseRetryDelay = 30 * time.Second

// PGWorker is a worker that is postgres-specific. It makes it easy to run
// arbitrary database commands within the same transaction as EntroQ management.
type PGWorker struct {
	Qs          []string
	MaxAttempts int32

	eqc *entroq.EntroQ

	lease          time.Duration
	baseRetryDelay time.Duration
}

// PGWorkerOption is an option for configuring a PGWorker.
type PGWorkerOption func(*PGWorker)

// WithLease sets the lease duration for tasks picked up by the worker.
// Note that this can be refreshed in the background. It sets a maximum time on
// task staleness if refresh fails.
func WithLease(d time.Duration) PGWorkerOption {
	return func(w *PGWorker) {
		w.lease = d
	}
}

// WithBaseRetryDelay sets the base retry delay for tasks picked up by the worker. This is the initial retry delay for a task that hits a retryable error, and the delay will
// grow exponentially with each subsequent retryable error up to a maximum of 10
// minutes.
func WithBaseRetryDelay(d time.Duration) PGWorkerOption {
	return func(w *PGWorker) {
		w.baseRetryDelay = d
	}
}

// NewPGWorker creates a new PGWorker against a set of queues. The worker will
// To use this requires slightly more work to set up if you want to get access
// to PostgreSQL transactions. (Don't discard errors as is done below!)
func NewPGWorker(eq *entroq.EntroQ, qs ...string) *PGWorker {
	return &PGWorker{
		Qs:             qs,
		eqc:            eq,
		lease:          DefaultLeaseTime,
		baseRetryDelay: DefaultBaseRetryDelay,
	}
}

// WithOpts sets options on a newly-created worker.
func (w *PGWorker) WithOpts(opts ...PGWorkerOption) *PGWorker {
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Work is a function called by Run. It does the actual handling of tasks pulled
// from the list of queues.
// It is passed the initial claimed task, and a function that can be used to
// stop renewal and obtain the final, version-stable task for modifications.
//
// The pattern within a function like this is
//   - Do potentially long-running work on task
//   - call stop, get the latest renewed task
//   - Do quick cleanup work (EntroQ Modify, database updates)
//
// It is harmless to leave stop uncalled.
type DoWork func(ctx context.Context, task *entroq.Task, stop entroq.FinalizeRenew) error

// Run starts the worker, calling the given work function once per claimed task.
func (w *PGWorker) Run(ctx context.Context, work DoWork) error {
	if len(w.Qs) == 0 {
		return fmt.Errorf("pgworker run: no queues specified")
	}

	// main loop
	for {
		task, err := w.eqc.Claim(ctx, entroq.From(w.Qs...), entroq.ClaimFor(w.lease))
		if err != nil {
			log.Printf("err in claim: %v", err)
			if entroq.IsCanceled(err) {
				log.Printf("claim was canceled")
				return nil
			}
			return fmt.Errorf("pgworker claim (%q): %w", w.Qs, err)
		}

		if err := w.eqc.DoWithRenew(ctx, task, w.lease, func(ctx context.Context, stop entroq.FinalizeRenew) error {
			if err := work(ctx, task, stop); err != nil {
				return fmt.Errorf("pgworker work: %w", err)
			}
			return nil
		}); err != nil {
			if _, ok := entroq.AsDependency(err); ok {
				log.Printf("Worker continuing after dependency (%q)", w.Qs)
				continue
			}
			if entroq.IsTimeout(err) {
				log.Printf("Worker continuing after timeout (%q)", w.Qs)
				continue
			}
			if entroq.IsCanceled(err) {
				log.Printf("Worker canceled: %v", err)
				return nil
			}
			return fmt.Errorf("worker error: %w", err)
		}
	}
}

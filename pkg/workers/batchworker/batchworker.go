package batchworker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
)

const backoffDuration = 5 * time.Second

// Worker[T] aggregates tasks and processes them in batches.
type Worker[T any] struct {
	eqc    *entroq.EntroQ
	outbox string
}

// New[T] creates a new batch worker with the given EntroQ client and outbox.
func New[T any](eq *entroq.EntroQ, outbox string) *Worker[T] {
	return &Worker[T]{
		eqc:    eq,
		outbox: outbox,
	}
}

// Run starts the batching loop.
func (bw *Worker[T]) Run(ctx context.Context, opts ...RunOption) error {
	ro := &runOpt{
		maxSize:     10,
		maxWait:     5 * time.Second,
		lease:       entroq.DefaultClaimDuration,
		gracePeriod: 1 * time.Second,
	}

	for _, opt := range opts {
		opt(ro)
	}

	if ro.lease == 0 {
		ro.lease = entroq.DefaultClaimDuration
	}
	if ro.maxWait > ro.lease/2 || ro.maxWait == 0 {
		ro.maxWait = ro.lease / 2
	}
	if ro.gracePeriod > ro.maxWait/2 || ro.gracePeriod == 0 {
		ro.gracePeriod = ro.maxWait / 2
	}
	if ro.maxSize > 1000 || ro.maxSize == 0 {
		ro.maxSize = 1000
	}

	if len(ro.qs) == 0 {
		return fmt.Errorf("batchworker: no queues specified")
	}

	for {
		tasks, values, err := bw.collectBatch(ctx, ro)
		if err != nil {
			if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
				return nil
			}
			log.Printf("Batch collection error: %v, backing off...", err)
			select {
			case <-ctx.Done():
				return fmt.Errorf("batchworker run: %w", ctx.Err())
			case <-time.After(backoffDuration):
			}
			continue
		}

		if len(tasks) == 0 {
			continue
		}

		var modArgs []entroq.ModifyArg
		for _, t := range tasks {
			modArgs = append(modArgs, t.Delete())
		}
		modArgs = append(modArgs, entroq.InsertingInto(bw.outbox, entroq.WithValue(values)))

		if _, err = bw.eqc.Modify(ctx, modArgs...); err != nil {
			log.Printf("Batch commit failed: %v", err)
			// Tasks will eventually time out and be retried.
		}
	}
}

func (bw *Worker[T]) collectBatch(ctx context.Context, ro *runOpt) ([]*entroq.Task, []T, error) {
	var tasks []*entroq.Task
	var values []T

	// First task (blocking)
	task, err := bw.eqc.Claim(ctx, entroq.From(ro.qs...), entroq.ClaimFor(ro.lease))
	if err != nil {
		return nil, nil, err
	}
	val, err := entroq.GetValue[T](task)
	if err != nil {
		return nil, nil, fmt.Errorf("batchworker: get value: %w", err)
	}
	tasks = append(tasks, task)
	values = append(values, val)

	if ro.maxSize == 1 {
		return tasks, values, nil
	}

	// Rest of the batch (limited wait)
	timer := time.NewTimer(ro.maxWait)
	defer timer.Stop()

	for len(values) < ro.maxSize {
		select {
		case <-ctx.Done():
			return tasks, values, nil
		case <-timer.C:
			return tasks, values, nil
		default:
			// Try to get another task quickly.
			// If we have at least one task, we don't block.
			// But wait! To implement the linger (grace period), we should wait a bit.
			innerCtx, cancel := context.WithTimeout(ctx, ro.gracePeriod)
			task, err := bw.eqc.Claim(innerCtx, entroq.From(ro.qs...), entroq.ClaimFor(ro.lease))
			cancel()

			if err != nil {
				if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
					return tasks, values, nil
				}
				return tasks, values, fmt.Errorf("batchworker: secondary claim: %w", err)
			}

			val, err := entroq.GetValue[T](task)
			if err != nil {
				return tasks, values, fmt.Errorf("batchworker: secondary value: %w", err)
			}
			tasks = append(tasks, task)
			values = append(values, val)

			// Reset grace period timer if we got something?
			// The original logic just kept going until maxWait or maxSize.
		}
	}

	return tasks, values, nil
}

type runOpt struct {
	qs          []string
	maxSize     int
	maxWait     time.Duration
	lease       time.Duration
	gracePeriod time.Duration
}

// RunOption is an option for Worker.Run.
type RunOption func(*runOpt)

// Watching specifies the queues to watch for tasks.
func Watching(qs ...string) RunOption {
	return func(o *runOpt) {
		o.qs = append(o.qs, qs...)
	}
}

// WithMaxSize specifies the maximum number of tasks to collect in a batch.
func WithMaxSize(n int) RunOption {
	return func(o *runOpt) {
		o.maxSize = n
	}
}

// WithMaxWait specifies the maximum time to wait for a batch to fill.
func WithMaxWait(d time.Duration) RunOption {
	return func(o *runOpt) {
		o.maxWait = d
	}
}

// WithLease specifies the lease duration for claimed tasks.
func WithLease(d time.Duration) RunOption {
	return func(o *runOpt) {
		o.lease = d
	}
}

// WithGracePeriod specifies the time to wait for another task after one is claimed.
func WithGracePeriod(d time.Duration) RunOption {
	return func(o *runOpt) {
		o.gracePeriod = d
	}
}

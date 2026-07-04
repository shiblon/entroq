package eqmem

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/queues"
)

// Default garbage-collection tuning. Not exposed as options: GC is a first-class,
// always-on backend behavior. Tests override the interval via withGCInterval.
const (
	defaultGCInterval  = time.Minute
	defaultGCBatchSize = 1000

	// gcClaimant is the claimant GC uses to acquire a task before deleting it.
	// GC goes through the front door (TryClaim then Modify-delete) rather than
	// mutating the queue's internal structures, so the claim is the mutex: a task
	// GC holds cannot be claimed by a worker, and vice versa.
	gcClaimant = "entroq-gc-collector"
)

// withGCInterval overrides the GC scan interval. Unexported: only in-package
// tests use it, to drive the loop fast enough to observe.
func withGCInterval(d time.Duration) Option {
	return func(m *EQMem) {
		m.gcInterval = d
	}
}

// runGCLoop drains queues that opt into garbage collection by name (a /gc=
// component) on an interval until ctx is canceled. Errors are logged and the
// loop continues; it never blocks the backend's own operations.
func (m *EQMem) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			m.reportMalformed(ctx) // once per sweep, before draining
			for {
				n, err := m.collectOnce(ctx, batch)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("eqmem gc: %v", err)
					}
					break
				}
				if n < batch {
					break // backlog drained, or nothing was due
				}
				if ctx.Err() != nil {
					return
				}
			}
			m.gcMetrics.Sweep(ctx, time.Since(start))
		}
	}
}

// reportMalformed surfaces queues that opted into GC (a /gc= component) but whose
// activation value will not parse: they are never collected, so without this they
// would pile up silently. Runs once per sweep, emitting the metric and a log line
// for each malformed queue.
func (m *EQMem) reportMalformed(ctx context.Context) {
	for _, q := range m.gcCandidateQueues() {
		_, present, err := queues.GCActivation(q)
		if !present || err == nil {
			continue // not a gc= queue, or a well-formed one
		}
		m.gcMetrics.Error(ctx, q, "malformed")
		log.Printf("eqmem gc: queue %q has a malformed gc= value; it will never be collected", q)
	}
}

// gcCandidateQueues returns the names of queues that carry a /gc= component. It
// holds the global lock only long enough to copy matching names with a cheap
// substring test -- the full activation parse happens outside the lock -- to keep
// contention with live claims/modifies to a minimum.
func (m *EQMem) gcCandidateQueues() []string {
	defer un(lock(m))
	var out []string
	for q := range m.queues {
		if strings.Contains(q, "/gc=") {
			out = append(out, q)
		}
	}
	return out
}

// collectOnce deletes up to batch collectable tasks from queues whose gc
// activation has passed, and returns the number deleted. It works through the
// public claim/modify path: discover gc= queues, keep those that are due (parsed
// outside the lock), shuffle to spread contention, then claim-and-delete tasks
// one queue at a time. Claiming is the mutex, so a task a worker holds is skipped
// rather than clobbered, and GC never blocks live traffic beyond the brief,
// per-operation locks that claim and modify already take.
func (m *EQMem) collectOnce(ctx context.Context, batch int) (int, error) {
	now := time.Now()

	var due []string
	for _, q := range m.gcCandidateQueues() {
		at, present, err := queues.GCActivation(q)
		if err == nil && present && !at.After(now) {
			due = append(due, q)
		}
	}
	rand.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })

	// Report per-queue deletions on every exit path (including errors) so a
	// partial pass still shows the work it did.
	perQueue := make(map[string]int)
	defer func() {
		for q, c := range perQueue {
			m.gcMetrics.Deleted(ctx, q, c)
		}
	}()

	deleted := 0
	for _, q := range due {
		for deleted < batch {
			task, err := m.TryClaim(ctx, &entroq.ClaimQuery{
				Queues:   []string{q},
				Claimant: gcClaimant,
				Duration: entroq.DefaultClaimDuration,
			})
			if err != nil {
				if ctx.Err() == nil { // don't count shutdown cancellation as a GC error
					m.gcMetrics.Error(ctx, q, "claim")
				}
				return deleted, fmt.Errorf("gc claim %q: %w", q, err)
			}
			if task == nil {
				break // nothing more collectable in this queue right now
			}
			if _, err := m.Modify(ctx, entroq.NewModification(gcClaimant, task.Delete())); err != nil {
				if ctx.Err() == nil {
					m.gcMetrics.Error(ctx, q, "delete")
				}
				return deleted, fmt.Errorf("gc delete %v: %w", task.IDVersion(), err)
			}
			perQueue[q]++
			deleted++
		}
		if deleted >= batch {
			break
		}
	}
	return deleted, nil
}

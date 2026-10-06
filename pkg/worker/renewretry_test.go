package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// flakyRenewalBackend fails the first failures renewals -- modifications that
// carry only a task arrival -- and records when each was attempted. Everything
// else passes through, so the claim itself and the commit are unaffected.
type flakyRenewalBackend struct {
	entroq.Backend
	failures int32

	mu       sync.Mutex
	attempts []time.Time
}

func (b *flakyRenewalBackend) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	renewal := len(mod.Arrives) > 0 && len(mod.Changes) == 0 && len(mod.Inserts) == 0 && len(mod.Deletes) == 0
	if !renewal {
		return b.Backend.Modify(ctx, mod)
	}
	b.mu.Lock()
	b.attempts = append(b.attempts, time.Now())
	b.mu.Unlock()
	if atomic.AddInt32(&b.failures, -1) >= 0 {
		// Not a dependency error: a lost claim stops the work, and this is the
		// transient kind that must be retried instead.
		return nil, fmt.Errorf("renewal is having a bad day")
	}
	return b.Backend.Modify(ctx, mod)
}

func (b *flakyRenewalBackend) gaps() []time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(b.attempts); i++ {
		out = append(out, b.attempts[i].Sub(b.attempts[i-1]))
	}
	return out
}

func flakyRenewalOpener(inner entroq.BackendOpener, failures int32, out **flakyRenewalBackend) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		b, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		fb := &flakyRenewalBackend{Backend: b, failures: failures}
		*out = fb
		return fb, nil
	}
}

// TestTransientRenewalRetriesInsideTheMargin drives a real renewal failure
// through the loop and checks WHEN the next attempt happened. The policy test
// above pins the arithmetic; this pins that the loop uses it, which is the part
// that was wrong: a transient failure waited the full interval, spending the
// whole margin on one more attempt.
func TestTransientRenewalRetriesInsideTheMargin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A 3s lease: interval 2s, margin 1s, so the first retry is due at 125ms
	// and the full interval would be 2s -- far apart enough to tell apart
	// without a tight timing assumption.
	const floor = 3 * time.Second
	var flaky *flakyRenewalBackend
	client, err := entroq.New(ctx, flakyRenewalOpener(leaseFloorOpener(eqmem.Opener(), floor), 1, &flaky))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const queue = "renewal_retry"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	task, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(floor))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	interval, margin := renewInterval(task), renewalMargin(task)

	var retries atomic.Int64
	// Work long enough for the first renewal, its failure, and the retry.
	work := interval + margin
	if _, err := doWhileRenewing(ctx, client, 0, held{task: task},
		func() { retries.Add(1) },
		func(ctx context.Context, stop finalizeRenew) error {
			defer stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(work):
			}
			return nil
		}); err != nil {
		t.Fatalf("doWhileRenewing: %v", err)
	}

	if got := retries.Load(); got != 1 {
		t.Errorf("Retries recorded: got %d, want 1", got)
	}
	gaps := flaky.gaps()
	if len(gaps) == 0 {
		t.Fatalf("Only %d renewal attempts; the retry never happened", len(gaps)+1)
	}
	// The gap between the failure and the retry is the whole point. Jitter adds
	// up to a quarter, so allow that and no more.
	first := margin / 8
	if gaps[0] > first+first/4+500*time.Millisecond {
		t.Errorf("Retry came %v after the failure, want about %v: the loop waited a cadence, not a retry (gaps %v)", gaps[0], first, gaps)
	}
	if gaps[0] >= interval {
		t.Errorf("Retry came %v after the failure, which is the full interval %v", gaps[0], interval)
	}
}

// TestNextRenewalRetryFitsInsideTheMargin covers the policy a transient
// renewal failure retries on. The margin is what the cadence deliberately
// leaves unused, and it is the whole budget for recovering: waiting the
// ordinary interval spends it on one attempt, so one dropped packet costs the
// claim.
func TestNextRenewalRetryFitsInsideTheMargin(t *testing.T) {
	// A 60s lease: the interval is 40s and the margin 20s.
	lease := 60 * time.Second
	now := time.Now()
	task := &entroq.Task{Modified: now, At: now.Add(lease)}
	interval, margin := renewInterval(task), renewalMargin(task)
	if interval != 40*time.Second || margin != 20*time.Second {
		t.Fatalf("Lease %v: interval %v, margin %v; the rest of this test assumes 40s and 20s", lease, interval, margin)
	}

	// Consecutive failures double the wait, starting at an eighth of the
	// margin, and stop growing at the interval.
	var delays []time.Duration
	var last time.Duration
	for range 6 {
		last = nextRenewalRetry(last, margin, interval)
		delays = append(delays, last)
	}
	want := []time.Duration{
		2500 * time.Millisecond, 5 * time.Second, 10 * time.Second,
		20 * time.Second, 40 * time.Second, 40 * time.Second,
	}
	for i, w := range want {
		if delays[i] != w {
			t.Errorf("Retry %d: got %v, want %v (all: %v)", i+1, delays[i], w, delays)
		}
	}

	// Three attempts land inside the margin, which is the point: a blip is
	// recovered from without the claim ever being at risk.
	var elapsed time.Duration
	inside := 0
	for _, d := range delays {
		elapsed += d
		if elapsed < margin {
			inside++
		}
	}
	if inside != 3 {
		t.Errorf("Attempts inside the %v margin: got %d, want 3 (delays %v)", margin, inside, delays)
	}

	// Growth stops at the interval, so an outage lasting minutes renews at the
	// ordinary rate rather than several times it.
	if got := nextRenewalRetry(interval, margin, interval); got != interval {
		t.Errorf("Retry after one already at the interval: got %v, want the interval %v", got, interval)
	}

	// A success returns to the ordinary cadence.
	if got := nextRenewalRetry(0, margin, interval); got != margin/8 {
		t.Errorf("First retry after a success: got %v, want %v", got, margin/8)
	}
}

// TestNextRenewalRetryWithNoMargin covers a lease that leaves nothing to retry
// inside, which a hand-built task with no arrival produces. There is no faster
// cadence to fall back to, so the ordinary one is the answer -- and in
// particular the wait is never zero, which would spin.
func TestNextRenewalRetryWithNoMargin(t *testing.T) {
	for _, tc := range []struct {
		name             string
		margin, interval time.Duration
	}{
		{"no margin", 0, 40 * time.Second},
		{"negative margin", -time.Second, 40 * time.Second},
		{"no lease at all", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextRenewalRetry(0, tc.margin, tc.interval); got != tc.interval {
				t.Errorf("Retry with margin %v and interval %v: got %v, want the interval", tc.margin, tc.interval, got)
			}
		})
	}
}

// TestRenewalMarginIsWhatTheCadenceLeaves ties the two together, so a change to
// the renewal fraction cannot silently leave the retry policy describing a
// margin that is not there.
func TestRenewalMarginIsWhatTheCadenceLeaves(t *testing.T) {
	now := time.Now()
	task := &entroq.Task{Modified: now, At: now.Add(90 * time.Second)}
	if got, want := renewalMargin(task)+renewInterval(task), grantedLease(task); got != want {
		t.Errorf("margin + interval = %v, want the granted lease %v", got, want)
	}
	if renewalMargin(task) <= 0 {
		t.Error("A positive lease must leave a positive margin, or a renewal has no time to complete in")
	}
}

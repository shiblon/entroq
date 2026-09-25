package eqtest

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// ReadinessFanout returns a Tester for a backend whose readiness loop runs at
// interval. Several waiters block on a queue whose tasks all become available
// at the same moment, with a claim poll too long to rescue them; every waiter
// must be woken within about one interval of the tasks maturing, not one waiter
// per interval.
func ReadinessFanout(interval time.Duration) Tester {
	return func(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
		const waiters = 4
		queue := path.Join(qPrefix, "readiness_fanout")

		claimed := make(chan error, waiters)
		for range waiters {
			go func() {
				_, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute), entroq.ClaimPollTime(time.Hour))
				claimed <- err
			}()
		}

		// Arrive in the future so the insert itself notifies no one; only the
		// readiness loop can wake the waiters.
		arrival := time.Now().Add(interval)
		var inserts []entroq.ModifyArg
		for range waiters {
			inserts = append(inserts, entroq.InsertingInto(queue, entroq.WithArrivalTime(arrival)))
		}
		if _, err := client.Modify(ctx, inserts...); err != nil {
			t.Fatalf("Insert future tasks: %v", err)
		}

		// One interval for the next tick after arrival, one more for slack.
		deadline := time.NewTimer(time.Until(arrival) + 2*interval)
		defer deadline.Stop()
		for i := range waiters {
			select {
			case err := <-claimed:
				if err != nil {
					t.Fatalf("Claim %d: %v", i, err)
				}
			case <-deadline.C:
				t.Fatalf("Only %d of %d waiters woke within two readiness intervals of arrival", i, waiters)
			}
		}
	}
}

package workgateway

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// claimsHandedOut runs up to sessions sessions in turn against one task, each
// registering with maxClaims, taking the task and hanging up without
// answering: a claim the task does not survive. It reports how many sessions
// were handed the task, and whether it reached the error queue.
func claimsHandedOut(ctx context.Context, t *testing.T, maxClaims int32, sessions int) (int, bool) {
	t.Helper()
	const lease = 100 * time.Millisecond
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	handed := 0
	for range sessions {
		r, w, c := osPipes(t)
		done := served(ctx, eq, r, w, WithLease(lease))
		c.send(&Request{Type: ReqConfig, Config: &Config{
			Queues: []string{"inbox"}, ProtocolVersions: []int32{1}, MaxClaims: maxClaims,
		}})
		ready := c.recv()
		expectType(t, ready, RespReady)
		c.send(&Request{Type: ReqReady, Session: ready.Session})

		next := make(chan *Response, 1)
		go func() {
			var msg Response
			if err := c.dec.Decode(&msg); err == nil {
				next <- &msg
			}
		}()
		var got *Response
		select {
		case got = <-next:
		case <-time.After(10 * lease):
		}
		c.hangUp()
		<-done
		// An idle ready is the gateway still waiting: nothing was handed out.
		if got == nil || got.Type == RespReady {
			break
		}
		expectType(t, got, RespWork)
		handed++
		time.Sleep(2 * lease) // the abandoned claim lapses
	}

	quarantined, err := eq.Tasks(ctx, "inbox/err")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	return handed, len(quarantined) == 1
}

// TestClaimLimitDefaultsToTheWorkers is a client that says nothing about the
// claim limit: it gets the Go worker's, as zero gets the default everywhere
// on this wire.
func TestClaimLimitDefaultsToTheWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handed, quarantined := claimsHandedOut(ctx, t, 0, worker.DefaultMaxClaims+2)
	if handed != worker.DefaultMaxClaims || !quarantined {
		t.Errorf("handed out %d times, quarantined %v; want %d, then quarantined", handed, quarantined, worker.DefaultMaxClaims)
	}
}

// TestNegativeClaimLimitIsNoLimit is a client that turns the limit off.
func TestNegativeClaimLimitIsNoLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const sessions = worker.DefaultMaxClaims + 2
	handed, quarantined := claimsHandedOut(ctx, t, -1, sessions)
	if handed != sessions || quarantined {
		t.Errorf("handed out %d times, quarantined %v; want %d, never quarantined", handed, quarantined, sessions)
	}
}

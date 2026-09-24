package eqmem

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

func TestClaimUnblocksAtFutureArrival(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, Opener(WithReadinessInterval(10*time.Millisecond)))
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer client.Close()

	const queue = "/test/future-readiness"
	arrival := time.Now().Add(200 * time.Millisecond)
	resp, err := client.Modify(ctx, entroq.InsertingInto(
		queue,
		entroq.WithArrivalTime(arrival),
		entroq.WithValue("future"),
	))
	if err != nil {
		t.Fatalf("insert future task: %v", err)
	}

	// This cannot return through polling before the test context expires. The
	// readiness notification must wake it after the task's arrival time.
	claimed, err := client.Claim(
		ctx,
		entroq.From(queue),
		entroq.ClaimPollTime(time.Hour),
	)
	if err != nil {
		t.Fatalf("claim future task: %v", err)
	}
	if claimed.ID != resp.InsertedTasks[0].ID {
		t.Fatalf("claimed task %q, want %q", claimed.ID, resp.InsertedTasks[0].ID)
	}
	if now := time.Now(); now.Before(arrival) {
		t.Fatalf("claimed future task at %v before arrival %v", now, arrival)
	}
}

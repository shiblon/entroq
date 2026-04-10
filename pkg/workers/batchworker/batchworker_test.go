package batchworker

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func TestWorker_proxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to open mem client: %v", err)
	}
	defer client.Close()

	// Insert 3 tasks
	for i := 1; i <= 3; i++ {
		if _, _, err := client.Modify(ctx, entroq.InsertingInto("/in", entroq.WithValue(i))); err != nil {
			t.Fatalf("Failed to insert task %d: %v", i, err)
		}
	}

	// Start worker in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- New[int](client, "/out").Run(ctx,
			WithMaxSize(3),
			WithMaxWait(2*time.Second),
			Watching("/in"),
		)
	}()

	// Wait for batch task in outbox
	task, err := client.Claim(ctx, entroq.From("/out"), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim batch task: %v", err)
	}

	batch, err := entroq.GetValue[[]int](task)
	if err != nil {
		t.Fatalf("Failed to unmarshal batch: %v", err)
	}

	sort.Ints(batch)

	expected := []int{1, 2, 3}
	for i, v := range expected {
		if batch[i] != v {
			t.Errorf("At index %d: expected %d, got %d", i, v, batch[i])
		}
	}

	cancel()
	<-errCh
}

func TestWorker_proxyLinger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to open mem client: %v", err)
	}
	defer client.Close()

	// Start worker in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- New[int](client, "/out").Run(ctx,
			WithMaxSize(10),
			WithMaxWait(2*time.Second),
			Watching("/in"),
			WithGracePeriod(250*time.Millisecond),
		)
	}()

	// Insert 1 task
	if _, _, err := client.Modify(ctx, entroq.InsertingInto("/in", entroq.WithValue(1))); err != nil {
		t.Fatalf("Failed to insert first task: %v", err)
	}

	// Wait a bit (less than grace period of 250ms)
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(100 * time.Millisecond):
	}

	// Insert 2nd task - should be caught by grace period
	if _, _, err := client.Modify(ctx, entroq.InsertingInto("/in", entroq.WithValue(2))); err != nil {
		t.Fatalf("Failed to insert second task: %v", err)
	}

	// Wait for batch task
	task, err := client.Claim(ctx, entroq.From("/out"), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim lingered batch: %v", err)
	}

	batch, err := entroq.GetValue[[]int](task)
	if err != nil {
		t.Fatalf("Failed to unmarshal batch: %v", err)
	}

	if len(batch) != 2 {
		t.Errorf("Expected batch of 2, got %d", len(batch))
	}

	cancel()
	<-errCh
}

package mapworker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func TestWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to open mem client: %v", err)
	}
	defer client.Close()

	// Insert a task
	input := Input[string]{
		Val:    "hello world",
		Outbox: "/out",
	}
	if _, err := client.Modify(ctx, entroq.InsertingInto("/in", entroq.WithValue(input))); err != nil {
		t.Fatalf("Failed to insert task: %v", err)
	}

	// Run worker in a goroutine (since it's a one-shot Run now)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		if err := Run(runCtx, client,
			func(ctx context.Context, s string) (string, error) {
				return strings.ToUpper(s), nil
			},
			WithQueues[string, string]("/in"),
		); err != nil && !entroq.IsCanceled(err) {
			t.Errorf("mapworker.Run failed: %v", err)
		}
	}()

	// Check output
	task, err := client.Claim(ctx, entroq.From("/out"), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim result: %v", err)
	}

	var output Output[string]
	if err := json.Unmarshal(task.Value, &output); err != nil {
		t.Fatalf("Failed to unmarshal output: %v", err)
	}

	if output.Val != "HELLO WORLD" {
		t.Errorf("Expected HELLO WORLD, got %q", output.Val)
	}
}

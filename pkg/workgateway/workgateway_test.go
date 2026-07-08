package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// TestBridge_OK drives the gateway bridge against a real in-memory EntroQ while
// this test plays the worker over in-memory pipes: it receives the work message,
// checks the task crossed the wire intact, replies "ok", and confirms the input
// task was committed (deleted).
func TestBridge_OK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const q = "in"
	if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("hello"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Gateway writes phase messages to gatewayW (worker reads clientR); worker
	// writes results to clientW (gateway reads gatewayR).
	clientR, gatewayW := io.Pipe()
	gatewayR, clientW := io.Pipe()
	bridge := NewBridge(NewPipeConn(gatewayR, gatewayW))

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	done := make(chan error, 1)
	go func() {
		w := worker.New(client, worker.WithDoModify[json.RawMessage](bridge.DoWork))
		done <- w.Run(runCtx, worker.Watching(q))
	}()

	// Play the worker: read one work message, reply ok.
	var wm struct {
		Type string      `json:"type"`
		Task entroq.Task `json:"task"`
	}
	if err := json.NewDecoder(clientR).Decode(&wm); err != nil {
		t.Fatalf("read work message: %v", err)
	}
	if wm.Type != "work" {
		t.Errorf("message type = %q, want %q", wm.Type, "work")
	}
	if got := string(wm.Task.Value); got != `"hello"` {
		t.Errorf("task value = %s, want %q", got, `"hello"`)
	}
	if wm.Task.Queue != q {
		t.Errorf("task queue = %q, want %q", wm.Task.Queue, q)
	}
	if err := json.NewEncoder(clientW).Encode(map[string]string{"type": "result", "outcome": "ok"}); err != nil {
		t.Fatalf("send result: %v", err)
	}

	// "ok" consumes the input, so the queue drains.
	if err := client.WaitQueuesEmpty(ctx, entroq.MatchExact(q)); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	runCancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("worker run: %v", err)
	}
}

package appendworker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

type mockAppender struct {
	sync.Mutex
	appended [][]byte
}

func (m *mockAppender) Append(b []byte) error {
	m.Lock()
	defer m.Unlock()
	m.appended = append(m.appended, b)
	return nil
}

func (m *mockAppender) Data() [][]byte {
	m.Lock()
	defer m.Unlock()
	var out [][]byte
	for _, b := range m.appended {
		out = append(out, b)
	}
	return out
}

func TestWorker_Run(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Setup in-memory EntroQ
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	// Add a task to be journaled
	queue := "/test/journal"
	data := []byte(`{"hello":"world"}`)
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue(json.RawMessage(data)))); err != nil {
		t.Fatalf("Failed to insert task: %v", err)
	}

	// Setup mock appender and worker
	appender := &mockAppender{}
	aw := New(appender)

	// Run worker in a separate goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- aw.Run(ctx, client, Watching(queue))
	}()

	// Wait for the task to be processed
	// Since the worker deletes the task, we can just poll the queue stats.
	for {
		stats, err := client.QueueStats(ctx)
		if err != nil {
			t.Fatalf("Failed to get stats: %v", err)
		}
		if s, ok := stats[queue]; !ok || (ok && s.Size == 0) {
			break
		}
		
		select {
		case err := <-errCh:
			t.Fatalf("Worker exited early: %v", err)
		case <-ctx.Done():
			t.Fatal("Timed out waiting for task to be processed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Verify the result
	appended := appender.Data()
	if len(appended) != 1 {
		t.Errorf("Expected 1 appended item, got %d", len(appended))
	} else if string(appended[0]) != string(data) {
		t.Errorf("Expected data %q, got %q", string(data), string(appended[0]))
	}
}

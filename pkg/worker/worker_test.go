package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func TestWorker_Basic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	if _, err := client.Modify(ctx, entroq.InsertingInto("test_q", entroq.WithValue("hi"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	done := make(chan bool, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		w := New(client,
			WithDoWork(func(ctx context.Context, task *entroq.Task, s string, _ []*entroq.Doc) error {
				if s != "hi" {
					return errors.New("wrong value")
				}
				return nil
			}),
			WithFinish(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) error {
				if _, err := client.Modify(ctx, task.Delete()); err != nil {
					return err
				}
				done <- true
				return nil
			}),
		)
		if err := w.Run(runCtx, Watching("test_q")); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Worker run: %v", err)
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for worker")
	}
}

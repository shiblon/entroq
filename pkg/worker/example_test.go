package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// Example_simpleWorker demonstrates a worker that processes tasks from a
// single queue and deletes them on completion. WithDoModify is the most
// common pattern: do work in one function, return the modifications to apply.
func Example_simpleWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Insert a task to be processed.
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("jobs", entroq.WithValue(map[string]any{"item": "hello"})),
	); err != nil {
		log.Fatalf("insert: %v", err)
	}

	done := make(chan struct{})

	w := worker.New[json.RawMessage](eq,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, value json.RawMessage, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
			fmt.Printf("processing task with value %s\n", value)
			// Return the modifications to apply atomically after work completes.
			return []entroq.ModifyArg{task.Delete()}, nil
		}),
	)

	go func() {
		defer close(done)
		if err := w.Run(ctx, worker.Watching("jobs")); err != nil && !entroq.IsCanceled(err) {
			log.Printf("worker: %v", err)
		}
	}()

	// Wait until the task is gone, then stop the worker.
	for {
		ts, err := eq.Tasks(ctx, "jobs")
		if err != nil {
			log.Fatalf("tasks: %v", err)
		}
		if len(ts) == 0 {
			cancel()
			break
		}
	}
	<-done

	// Output:
	// processing task with value {"item":"hello"}
}

// Example_workerWithDocs demonstrates a worker that claims a set of docs
// before doing work. The doc key comes from the task value, making the task
// an implicit mutex: only one worker ever holds the task, so only one worker
// ever tries to claim those docs.
func Example_workerWithDocs() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Create state docs and a task referencing their primary key.
	_, err = eq.Modify(ctx,
		entroq.CreatingIn("counters",
			entroq.WithKeys("totals", "requests"),
			entroq.WithContent(0),
		),
		entroq.CreatingIn("counters",
			entroq.WithKeys("totals", "errors"),
			entroq.WithContent(0),
		),
		entroq.InsertingInto("jobs", entroq.WithValue(map[string]any{
			"ns":  "counters",
			"key": "totals",
		})),
	)
	if err != nil {
		log.Fatalf("setup: %v", err)
	}

	type jobValue struct {
		NS  string `json:"ns"`
		Key string `json:"key"`
	}

	done := make(chan struct{})

	w := worker.New[jobValue](eq,
		worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, val jobValue) ([]*entroq.DocClaim, error) {
			// Declare which docs to claim before work begins.
			return []*entroq.DocClaim{{
				Namespace: val.NS,
				Key:       val.Key,
			}}, nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, val jobValue, docs []*entroq.Doc) ([]entroq.ModifyArg, error) {
			// docs are sorted by (primary key, secondary key).
			for _, d := range docs {
				fmt.Printf("doc key=%s secondary=%s\n", d.Key, d.SecondaryKey)
			}
			// Atomically delete the task and update the first doc.
			var n int
			if err := json.Unmarshal(docs[0].Content, &n); err != nil {
				return nil, fmt.Errorf("unmarshal: %w", err)
			}
			return []entroq.ModifyArg{
				task.Delete(),
				docs[0].Change(entroq.WithContent(n + 1)),
			}, nil
		}),
	)

	go func() {
		defer close(done)
		if err := w.Run(ctx, worker.Watching("jobs")); err != nil && !entroq.IsCanceled(err) {
			log.Printf("worker: %v", err)
		}
	}()

	for {
		ts, err := eq.Tasks(ctx, "jobs")
		if err != nil {
			log.Fatalf("tasks: %v", err)
		}
		if len(ts) == 0 {
			cancel()
			break
		}
	}
	<-done

	// Verify the doc was updated.
	docs, err := eq.Docs(context.Background(), &entroq.DocQuery{
		Namespace: "counters",
		KeyStart:  "totals",
		KeyEnd:    "totals\x00",
	})
	if err != nil {
		log.Fatalf("docs: %v", err)
	}
	for _, d := range docs {
		var n int
		if err := json.Unmarshal(d.Content, &n); err != nil {
			log.Fatalf("unmarshal: %v", err)
		}
		fmt.Printf("after: secondary=%s value=%d\n", d.SecondaryKey, n)
	}

	// Output:
	// doc key=totals secondary=errors
	// doc key=totals secondary=requests
	// after: secondary=errors value=1
	// after: secondary=requests value=0
}

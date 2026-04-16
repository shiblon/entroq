package eqtest

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

func SimpleWorker(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "simple_worker")

	attempts := 20
	if testing.Short() {
		attempts = 5
	}
	for i := 0; i < attempts; i++ {
		q := fmt.Sprintf("%s_%d", queue, i)
		simpleWorkerOnce(ctx, t, client, q)
	}
}

func simpleWorkerOnce(ctx context.Context, t *testing.T, client *entroq.EntroQ, queue string) {
	t.Helper()

	const numTasks = 10

	showQueue := func() {
		queues, err := client.Queues(context.Background())
		if err != nil {
			t.Fatalf("Error getting queues: %v", err)
		}
		log.Printf("Queues: %v", queues)
		tasks, err := client.Tasks(context.Background(), queue)
		if err != nil {
			t.Fatalf("Error getting queue contents: %v", err)
		}
		log.Printf("**** Queue %v (%v) ****\n", queue, len(tasks))
		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].ID < tasks[j].ID
		})
		for _, t := range tasks {
			log.Printf("  %v", t)
		}
	}

	ctx, cancel := context.WithCancel(ctx)

	g, ctx := errgroup.WithContext(ctx)

	var consumed []*entroq.Task
	g.Go(func() error {
		return worker.New(client,
			worker.WithDoWork(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
				if task.Claims != 1 {
					return fmt.Errorf("worker claim expected claims to be 1, got %d", task.Claims)
				}
				consumed = append(consumed, task)
				return nil
			}),
			worker.WithFinish(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
				_, err := client.Modify(ctx, task.Delete())
				return err
			}),
		).Run(ctx, worker.Watching(queue))
	})

	// Brief pause to let the worker goroutine reach its Claim call before we
	// insert tasks. This exercises the notify-on-insert wakeup path. 10ms is
	// ample for an in-process backend; it was previously 1s (20s total for
	// 20 iterations).
	select {
	case <-time.After(10 * time.Millisecond):
	case <-ctx.Done():
		t.Fatalf("Sleep: %v", ctx.Err())
	}

	var inserted []*entroq.Task
	for i := 0; i < numTasks; i++ {
		resp, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithRawValue(json.RawMessage(fmt.Sprintf("%d", i)))))
		if err != nil {
			t.Fatalf("Failed to insert task: %v", err)
		}
		inserted = append(inserted, resp.InsertedTasks...)
		select {
		case <-ctx.Done():
			t.Fatalf("Canceled while inserting: %v", ctx.Err())
		default:
		}
	}

	if got := len(inserted); got != numTasks {
		t.Fatalf("Inserted %v tasks, expected %v", got, numTasks)
	}

	if err := client.WaitQueuesEmpty(ctx, entroq.MatchExact(queue)); err != nil {
		t.Fatalf("Wait for queue empty: %v", err)
	}

	cancel()
	if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
		t.Fatalf("Worker exit error: %v", err)
	}

	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, consumed, expectVersionIncr(1)); diff != "" {
		showQueue()
		t.Errorf("Tasks inserted not the same as tasks consumed (-want +got):\n%v", diff)
	}
}

func MultiWorker(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	bigQueue := path.Join(qPrefix, "multi_worker_big")
	medQueue := path.Join(qPrefix, "multi_worker_medium")
	smallQueue := path.Join(qPrefix, "multi_worker_small")

	const (
		bigSize   = 300
		medSize   = 60
		smallSize = 20

		numWorkers = 5
	)

	// Populate all of the queues, most in the big one, least in the small one.
	for i := 0; i < bigSize; i++ {
		args := []entroq.ModifyArg{
			entroq.InsertingInto(bigQueue, entroq.WithValue("big value")),
		}
		if i < medSize {
			args = append(args, entroq.ModifyArg(
				entroq.InsertingInto(medQueue, entroq.WithValue("med value")),
			))
		}
		if i < smallSize {
			args = append(args, entroq.ModifyArg(
				entroq.InsertingInto(smallQueue, entroq.WithValue("smallvalue")),
			))
		}
		if _, err := client.Modify(ctx, args...); err != nil {
			t.Fatalf("Insert queues failed: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Keep track of what was consumed when.
	var consumed []*entroq.Task
	consumedCh := make(chan *entroq.Task)

	g, ctx := errgroup.WithContext(ctx)

	for range numWorkers {
		g.Go(func() error {
			ti := 0
			w := worker.New(client,
				worker.WithDoWork(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
					ti++
					if task.Claims != 1 {
						return fmt.Errorf("worker claim expected to be 1, was %d", task.Claims)
					}
					consumedCh <- task
					return nil
				}),
				worker.WithFinish(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
					_, err := client.Modify(ctx, task.Delete())
					return err
				}),
			)
			err := w.Run(ctx, worker.Watching(bigQueue, medQueue, smallQueue))
			if entroq.IsCanceled(err) {
				return nil
			}
			return err
		})
	}

	g.Go(func() error {
		waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Minute)
		defer waitCancel()
		if err := client.WaitQueuesEmpty(waitCtx, entroq.MatchExact(bigQueue, medQueue, smallQueue)); err != nil {
			return fmt.Errorf("waiting for empty queues: %w", err)
		}
		// All done. Stop the workers.
		cancel()
		return nil
	})

	go func() {
		g.Wait()
		close(consumedCh)
	}()

	for task := range consumedCh {
		consumed = append(consumed, task)
	}

	if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
		t.Fatalf("Error in worker: %v", err)
	}

	// Now check that we consumed the right tasks from the right queues.
	queuesFound := make(map[string]int)
	lastSmall, lastMed := -1, -1

	for i, t := range consumed {
		queuesFound[t.Queue]++
		switch t.Queue {
		case medQueue:
			lastMed = i
		case smallQueue:
			lastSmall = i
		}
	}

	if found := queuesFound[bigQueue]; found != bigSize {
		t.Errorf("Expected to consume %d from big queue, consumed %d", bigSize, found)
	}
	if found := queuesFound[medQueue]; found != medSize {
		t.Errorf("Expected to consume %d from med queue, consumed %d", medSize, found)
	}
	if found := queuesFound[smallQueue]; found != smallSize {
		t.Errorf("Expected to consume %d from small queue, consumed %d", smallSize, found)
	}

	total := len(consumed)

	// With fair multi-queue selection from 3 queues, each queue is chosen with
	// probability ~1/3. Small (20 tasks) should drain after roughly 20*3 = 60
	// total claims. We allow up to total/3 as a generous upper bound -- a
	// correct implementation should land well under this. A broken
	// implementation that always drains big first would fail badly (lastSmall
	// near position 380).
	if lastSmall >= total/3 {
		t.Errorf("small queue not exhausted fairly: last small task at position %d/%d (threshold %d)",
			lastSmall, total, total/3)
	}

	// Med (60 tasks) should drain after roughly 60+20*3 = 120 total claims
	// (accounting for the small-queue phase). We allow up to 3*total/4.
	if lastMed >= total*3/4 {
		t.Errorf("med queue not exhausted fairly: last med task at position %d/%d (threshold %d)",
			lastMed, total, total*3/4)
	}
}

// WorkerRetryOnError test that workers that have RetryTaskError results
// increment attempts and set the error properly. It also checks that after max
// attempts, things get moved.
func WorkerRetryOnError(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	newTask := func(val string) *entroq.Task {
		b, _ := json.Marshal(val)
		return &entroq.Task{
			Queue: path.Join(qPrefix, "retry_on_error", val),
			ID:    client.GenID(),
			Value: b,
		}
	}

	type tc struct {
		name        string
		input       *entroq.Task
		maxAttempt  int32
		wantAttempt int32
		wantErr     string
		wantMove    bool
	}

	cases := []tc{
		{
			name:        "retry-no-max",
			input:       newTask("retry"),
			maxAttempt:  0,
			wantAttempt: 1,
			wantErr:     `worker error ("retry"): worker retry`,
			wantMove:    false,
		},
		{
			name:        "retry-larger-max",
			input:       newTask("with max"),
			maxAttempt:  3,
			wantAttempt: 1,
			wantErr:     `worker error ("with max"): worker retry`,
			wantMove:    false,
		},
		{
			name:        "retry-too-many",
			input:       newTask("too many"),
			maxAttempt:  1,
			wantAttempt: 1,
			wantErr:     `worker error ("too many"): worker retry`,
			wantMove:    true,
		},
	}

	runWorkerOneCase := func(ctx context.Context, c tc) {
		t.Helper()

		// Keep track of the retried task - after we get past attempt 0, we
		// store it here. Then, if the task was not meant to be moved, we can
		// see what happened without a separate claim section.
		retriedTaskCh := make(chan *entroq.Task, 1)

		w := worker.New(client,
			worker.WithDoWork(func(ctx context.Context, task *entroq.Task, s string, _ []*entroq.Doc) error {
				// Only attempt this again if it's the first time.
				if task.Attempt == 0 {
					return fmt.Errorf("worker error (%q): %w", s, worker.RetryError)
				}
				// Save it so we know what happened with the retry error.
				retriedTaskCh <- task
				return nil
			}),
			worker.WithFinish(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) error {
				_, err := client.Modify(ctx, task.Delete())
				return err
			}),
		)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return w.Run(gctx, worker.Watching(c.input.Queue),
				worker.WithBaseRetryDelay(0),
				worker.WithMaxAttempts(c.maxAttempt),
			)
		})

		// Now stick our task in. The worker is ready and waiting.
		if _, err := client.Modify(ctx, entroq.InsertingInto(c.input.Queue, entroq.WithID(c.input.ID), entroq.WithRawValue(c.input.Value))); err != nil {
			t.Fatalf("Test %q insert task: %v", c.name, err)
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		var changedTask *entroq.Task
		if c.wantMove {
			// Expect the queue to become empty, get stuff out of the error queue.
			if err := client.WaitQueuesEmpty(waitCtx, entroq.MatchExact(c.input.Queue)); err != nil && !entroq.IsCanceled(err) {
				t.Fatalf("Test %q expected queue %q to become empty, didn't happen: %v", c.name, c.input.Queue, err)
			}
			// Now check that it's in the error queue and looks okay.
			errTasks, err := client.Tasks(ctx, w.ErrorQueueFor(c.input.Queue))
			if err != nil {
				t.Fatalf("Test %q can't get tasks from error queue: %v", c.name, err)
			}
			if want, got := 1, len(errTasks); want != got {
				t.Fatalf("Test %q expected %d error tasks, got %d", c.name, want, got)
			}
			changedTask = errTasks[0]
		} else {
			// Not moved, so we must have gotten it in the retriedTaskCh channel. Block on that for a bit.
			select {
			case changedTask = <-retriedTaskCh:
				if want, got := c.input.ID, changedTask.ID; want != got {
					t.Fatalf("Test %q retry task expected ID %v, got %v", c.name, want, got)
				}
			case <-time.After(5 * time.Second): // should be plenty of time for the worker to go round a couple of times.
				t.Fatalf("Test %q took too long getting the retried task from something that had multiple attempts", c.name)
			}
		}
		if want, got := c.wantErr, changedTask.Err; want != got {
			t.Fatalf("Test %q expected err %q, got %q", c.name, want, got)
		}
		if want, got := c.wantAttempt, changedTask.Attempt; want != got {
			t.Fatalf("Test %q expected attempt %d, got %d", c.name, want, got)
		}

		cancel()

		if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
			t.Fatalf("Test %q failed worker wait after cancel: %v", c.name, err)
		}
	}

	for _, test := range cases {
		runWorkerOneCase(ctx, test)
	}
}

// WorkerMoveOnError tests that workers that have MoveTaskError results,
// causing tasks to be moved instead of crashing.
func WorkerMoveOnError(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	baseQueue := path.Join(qPrefix, "move_on_error")

	type tc struct {
		name  string
		input *entroq.Task
		die   bool
		moved bool
	}

	newTask := func(val string) *entroq.Task {
		id := client.GenID()
		b, _ := json.Marshal(val)
		return &entroq.Task{
			Queue: path.Join(baseQueue, val, id),
			ID:    id,
			Value: b,
		}
	}

	cases := []tc{
		{
			name:  "die",
			input: newTask("die"),
			die:   true,
		},
		{
			name:  "move",
			input: newTask("move"),
			moved: true,
		},
		{
			name:  "wait-for-renewal",
			input: newTask("move-wait"),
			moved: true,
		},
	}

	runWorkerOneCase := func(t *testing.T, ctx context.Context, c tc) {
		t.Helper()
		// Regenerate ID each iteration so re-runs don't collide with tasks
		// left over from previous iterations (e.g. in the error queue).
		c.input.ID = client.GenID()

		const leaseTime = 2 * time.Second

		w := worker.New(client,
			worker.WithDoWork(func(ctx context.Context, task *entroq.Task, cmd string, _ []*entroq.Doc) error {
				switch cmd {
				case "die":
					return fmt.Errorf("task asked to die: %w", worker.FatalError)
				case "move":
					return fmt.Errorf("task asked to move: %w", worker.MoveError)
				case "move-wait":
					select {
					case <-time.After(leaseTime):
						return fmt.Errorf("task asked to move after renewal: %w", worker.MoveError)
					case <-ctx.Done():
						return fmt.Errorf("oops - test %q took too long, gave up before finishing: %w", c.name, ctx.Err())
					}
				}
				return nil
			}),
			worker.WithFinish(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) error {
				if _, err := client.Modify(ctx, task.Delete()); err != nil {
					return fmt.Errorf("task deletion failed: %w", err)
				}
				return nil
			}),
		)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			if err := w.Run(gctx, worker.Watching(c.input.Queue), worker.WithLease(leaseTime), worker.WithBackoff(100*time.Millisecond)); err != nil && !entroq.IsCanceled(err) {
				// Log quickly so we can see it before waits fail below.
				log.Printf("Worker Run error: %v", err)
				return err
			}
			return nil
		})

		if _, err := client.Modify(ctx, entroq.InsertingInto(c.input.Queue,
			entroq.WithID(c.input.ID),
			entroq.WithRawValue(c.input.Value),
		)); err != nil {
			t.Fatalf("Test %q insert task work: %v", c.name, err)
		}

		if c.die {
			if err := g.Wait(); err != nil && entroq.IsTimeout(err) {
				t.Fatalf("Test %q expected to die, but not with a timeout error: %v", c.name, err)
			}
			// Delete the dead task, will always be version 1.
			// Note: don't overwrite like this in real use.
			c.input.Version = 1
			if _, err := client.Modify(ctx, c.input.Delete()); err != nil {
				t.Fatalf("Test %q tried to clean up dead task: %v", c.name, err)
			}
			return
		}

		waitCtx, waitCancel := context.WithTimeout(ctx, 5*leaseTime)
		defer waitCancel()
		if err := client.WaitQueuesEmpty(waitCtx, entroq.MatchExact(c.input.Queue)); err != nil && !entroq.IsCanceled(err) {
			t.Fatalf("Test %q: no moved tasks found, task was not expected to die: %v", c.name, err)
		}
		errTasks, err := client.Tasks(ctx, w.ErrorQueueFor(c.input.Queue))
		if err != nil {
			t.Fatalf("Test %q find in error queue: %v", c.name, err)
		}
		var foundTask *entroq.Task
		for _, task := range errTasks {
			if task.ID == c.input.ID {
				foundTask = task
				if !strings.Contains(foundTask.Err, "asked to move") {
					t.Fatalf("Test %q expected moved task to have a move error, had %q", c.name, foundTask.Err)
				}
				break
			}
		}
		if c.moved && foundTask == nil {
			t.Errorf("Test %q expected task to be moved, but is not found in %q", c.name, w.ErrorQueueFor(c.input.Queue))
		} else if !c.moved && foundTask != nil {
			t.Errorf("Test %q expected task to be deleted, but showed up in %q", c.name, w.ErrorQueueFor(c.input.Queue))
		}

		cancel()

		err = g.Wait()
		if c.moved && err != nil && !entroq.IsCanceled(err) {
			t.Errorf("Test %q expected no error on move, got %v", c.name, err)
		} else if !c.moved && entroq.IsCanceled(err) {
			t.Errorf("Test %q expected error from worker, got none", c.name)
		}
	}

	stressCount := 5
	if testing.Short() {
		stressCount = 1
	}

	// Feed test cases one at a time to the worker, wait for empty, then
	// depending on desired outcomes, check error queue for expected value.
	for _, test := range cases {
		for i := 0; i < stressCount; i++ {
			t.Run(fmt.Sprintf("case=%v-%v", test.name, i), func(st *testing.T) {
				runWorkerOneCase(st, ctx, test)
			})
		}
	}
}

// WorkerRenewal tests that task claims are renewed periodically for longer-running work tasks.
func WorkerRenewal(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_renewal")

	_, err := client.Modify(ctx, entroq.InsertingInto(queue))
	if err != nil {
		t.Fatalf("Error inserting: %v", err)
	}

	// Newly-inserted task will have version 0.

	task, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(6*time.Second))
	if err != nil {
		t.Fatalf("Failed to claim task: %v", err)
	}

	// Task now has version 1.

	if err := worker.DoWithRenew(ctx, client, task, 6*time.Second, func(ctx context.Context, stop worker.FinalizeRenew) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker do with renew: %w", ctx.Err())
		case <-time.After(10 * time.Second): // long enough for 3 renewals.
		}
		renewed := stop()
		if want, got := task.Version+3, renewed.Version; want != got {
			t.Fatalf("Expected renewed task to be at version %d, got %d", want, got)
		}
		return nil
	}); err != nil {
		t.Fatalf("Error renewing and waiting: %v", err)
	}
}

// ClaimUnblocksOnNotify verifies that Claim wakes promptly when a task is
// inserted rather than waiting the full poll interval. Backends that implement
// a NotifyWaiter (eqpg via LISTEN/NOTIFY, eqmem via in-process signaling)
// should pass easily; a poll-only backend would time out.
func WorkerDependencyHandler(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_dep_handler")
	confQueue := path.Join(queue, "config")

	// Insert a task.
	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithValue("dep-task")),
		entroq.InsertingInto(confQueue), // empty task to depend on
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	inserted := resp.InsertedTasks
	_ = inserted[0]
	confTask := inserted[1]

	// Signaling channels.
	inWork := make(chan bool)
	letFinish := make(chan bool)
	handlerCalled := make(chan bool, 1)

	w := worker.New(client,
		worker.WithDoWork(func(ctx context.Context, task *entroq.Task, val string, _ []*entroq.Doc) error {
			inWork <- true
			<-letFinish
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, task *entroq.Task, val string, _ []*entroq.Doc) error {
			// Try to delete the task. This will fail because we'll modify it in the main thread.
			_, err := client.Modify(ctx, task.Delete(), confTask.Depend())
			return err
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, worker.Watching(queue),
			worker.WithDependencyHandler(func(ctx context.Context, task *entroq.Task, de *entroq.DependencyError) error {
				handlerCalled <- true
				return nil
			}))
	}()

	// Wait for worker to be in work.
	select {
	case <-inWork:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for work to start")
	}

	// Now modify the config task in the main thread to force a version mismatch for the worker.
	if _, err := client.Modify(ctx, confTask.Change(entroq.ArrivalTimeBy(2*time.Second))); err != nil {
		t.Fatalf("Main thread modify: %v", err)
	}

	// Release the worker to let it try to finish.
	letFinish <- true

	// Verify handler is called.
	select {
	case <-handlerCalled:
	case err := <-errCh:
		t.Fatalf("Worker exited before handler called: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for dependency handler")
	}

	runCancel()
	<-errCh
}

// WorkerCompactDependencyHandler tests that the bubble-up logic works for WithDoModify.
func WorkerCompactDependencyHandler(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_compact_dep_handler")
	confQueue := path.Join(queue, "config")

	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithValue("compact-dep-task")),
		entroq.InsertingInto(confQueue), // just an empty task to depend on
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	inserted := resp.InsertedTasks
	_ = inserted[0]
	confTask := inserted[1]

	inWork := make(chan bool)
	letFinish := make(chan bool)
	handlerCalled := make(chan bool, 1)

	w := worker.New(client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, val string, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
			inWork <- true
			<-letFinish
			return []entroq.ModifyArg{task.Delete(), confTask.Depend()}, nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, worker.Watching(queue),
			worker.WithDependencyHandler(func(ctx context.Context, task *entroq.Task, de *entroq.DependencyError) error {
				handlerCalled <- true
				return nil
			}))
	}()

	select {
	case <-inWork:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for work to start")
	}

	// Alter the config task so that the dependency fails.
	if _, err := client.Modify(ctx, confTask.Change(entroq.ArrivalTimeBy(2*time.Second))); err != nil {
		t.Fatalf("Modify dependency: %v", err)
	}

	letFinish <- true

	select {
	case <-handlerCalled:
	case err := <-errCh:
		t.Fatalf("Worker exited before handler called: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for dependency handler")
	}

	runCancel()
	if err := <-errCh; err != nil && !entroq.IsCanceled(err) {
		t.Errorf("worker exit: %v", err)
	}
}

// WorkerRenewalNoDependencyHandler verifies that renewal failures don't trigger the hook.
func WorkerRenewalNoDependencyHandler(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_renewal_no_dep")

	resp, err := client.Modify(ctx, entroq.InsertingInto(queue))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	task := resp.InsertedTasks[0]

	inWork := make(chan bool)
	handlerCalled := make(chan bool, 1)

	// Lease shorter than our wait time to force renewal.
	const lease = 2 * time.Second

	w := worker.New(client,
		worker.WithDoWork(func(ctx context.Context, task *entroq.Task, val string, _ []*entroq.Doc) error {
			inWork <- true
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(4 * time.Second): // Wait long enough for renewal attempt.
			}
			return nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, worker.Watching(queue),
			worker.WithDependencyHandler(func(ctx context.Context, task *entroq.Task, de *entroq.DependencyError) error {
				t.Errorf("Dependency handler should NOT be called for renewal failure!")
				handlerCalled <- true
				return nil
			}))
	}()

	select {
	case <-inWork:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for work to start")
	}

	// Modify the task to break renewal.
	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("No tasks found to modify")
	}
	task = tasks[0]

	if _, err := client.Modify(ctx, task.Change(entroq.RawValueTo([]byte("\"broken-renewal\"")))); err != nil {
		t.Fatalf("Main thread modify: %v", err)
	}

	// Wait for renewal to fail and worker to notice.
	// Since we expect NO handler call, we just wait for a bit.
	select {
	case <-handlerCalled:
		t.Fatal("Dependency handler called for renewal failure!")
	case err := <-errCh:
		t.Fatalf("Worker exited before handler called: %v", err)
	case <-time.After(5 * time.Second):
		// Success: handler wasn't called.
	}
}

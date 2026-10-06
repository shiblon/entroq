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
			worker.WithDoWork(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.DocSet) error {
				if task.Claims != 1 {
					return fmt.Errorf("worker claim expected claims to be 1, got %d", task.Claims)
				}
				consumed = append(consumed, task)
				return nil
			}),
			worker.WithFinish(func(ctx context.Context, mod worker.Modifier, task *entroq.Task, _ json.RawMessage, _ []*entroq.DocSet) error {
				_, err := mod.Modify(ctx, task.Delete())
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
	for i := range numTasks {
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
	for i := range bigSize {
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
				worker.WithDoWork(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.DocSet) error {
					ti++
					if task.Claims != 1 {
						return fmt.Errorf("worker claim expected to be 1, was %d", task.Claims)
					}
					consumedCh <- task
					return nil
				}),
				worker.WithFinish(func(ctx context.Context, mod worker.Modifier, task *entroq.Task, _ json.RawMessage, _ []*entroq.DocSet) error {
					_, err := mod.Modify(ctx, task.Delete())
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
			wantErr:     `worker error ("retry")`,
			wantMove:    false,
		},
		{
			name:        "retry-larger-max",
			input:       newTask("with max"),
			maxAttempt:  3,
			wantAttempt: 1,
			wantErr:     `worker error ("with max")`,
			wantMove:    false,
		},
		{
			name:        "retry-too-many",
			input:       newTask("too many"),
			maxAttempt:  1,
			wantAttempt: 1,
			wantErr:     `worker error ("too many")`,
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
			worker.WithDoWork(func(ctx context.Context, task *entroq.Task, s string, _ []*entroq.DocSet) error {
				// Only attempt this again if it's the first time.
				if task.Attempt == 0 {
					return worker.RetryErrorf("worker error (%q)", s)
				}
				// Save it so we know what happened with the retry error.
				retriedTaskCh <- task
				return nil
			}),
			worker.WithFinish(func(ctx context.Context, mod worker.Modifier, task *entroq.Task, _ string, _ []*entroq.DocSet) error {
				_, err := mod.Modify(ctx, task.Delete())
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
			worker.WithDoWork(func(ctx context.Context, task *entroq.Task, cmd string, _ []*entroq.DocSet) error {
				switch cmd {
				case "die":
					return worker.FatalErrorf("task asked to die")
				case "move":
					return worker.MoveErrorf("task asked to move")
				case "move-wait":
					select {
					case <-time.After(leaseTime):
						return worker.MoveErrorf("task asked to move after renewal")
					case <-ctx.Done():
						return fmt.Errorf("oops - test %q took too long, gave up before finishing: %w", c.name, ctx.Err())
					}
				}
				return nil
			}),
			worker.WithFinish(func(ctx context.Context, mod worker.Modifier, task *entroq.Task, _ string, _ []*entroq.DocSet) error {
				if _, err := mod.Modify(ctx, task.Delete()); err != nil {
					return fmt.Errorf("task deletion failed: %w", err)
				}
				return nil
			}),
		)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			if err := w.Run(gctx, worker.Watching(c.input.Queue), worker.AsClaimant(dieHolder), worker.WithLease(leaseTime)); err != nil && !entroq.IsCanceled(err) {
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
			//
			// As the Run's own claimant: a worker that died still HOLDS its
			// task until the lease lapses, and every Run is its own consumer,
			// so the connection cannot touch it. Naming the holder is what
			// lets this clean up without waiting out a lease.
			c.input.Version = 1
			if _, err := client.As(dieHolder).Modify(ctx, c.input.Delete()); err != nil {
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

// dieHolder names the consumer the worker in WorkerMoveOnError holds its task
// as, so the "die" case can clean up a task its dead worker still holds. A Run
// otherwise picks its own name and nothing outside it could say what that was.
const dieHolder = "eqtest-move-on-error"

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
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, val string, _ []*entroq.DocSet) (*worker.Result, error) {
			inWork <- true
			<-letFinish
			return worker.
				Modify(task.Delete(), confTask.Depend()).
				OnDependency(func(context.Context, *entroq.DependencyError) error {
					handlerCalled <- true
					return nil
				}), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, worker.Watching(queue))
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

// WorkerDependencyMove verifies that an OnDependency handler returning a
// MoveError quarantines the still-claimed input task. The commit fails on some
// OTHER dependency (a separate config task), so the input task is not implicated
// and remains validly claimed; the handler judges the failure unrecoverable and
// moves the task to the error queue rather than leaving it to be reclaimed.
func WorkerDependencyMove(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_dep_move")
	confQueue := path.Join(queue, "config")
	errQueue := path.Join(queue, "errors")

	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithValue("dep-move-task")),
		entroq.InsertingInto(confQueue), // a separate task to depend on
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	inputTask := resp.InsertedTasks[0]
	confTask := resp.InsertedTasks[1]

	inWork := make(chan bool)
	letFinish := make(chan bool)

	w := worker.New(client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, val string, _ []*entroq.DocSet) (*worker.Result, error) {
			inWork <- true
			<-letFinish
			return worker.
				Modify(task.Delete(), confTask.Depend()).
				OnDependency(func(context.Context, *entroq.DependencyError) error {
					return worker.MoveErrorf("dependency gone, quarantining").To(errQueue)
				}), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(runCtx, worker.Watching(queue)) }()

	select {
	case <-inWork:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for work to start")
	}

	// Break the OTHER dependency, leaving the input task validly claimed, so the
	// commit fails but the input task can still be moved by the handler.
	if _, err := client.Modify(ctx, confTask.Change(entroq.ArrivalTimeBy(2*time.Second))); err != nil {
		t.Fatalf("Modify dependency: %v", err)
	}

	letFinish <- true

	// The handler's MoveError should quarantine the input task into errQueue.
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := client.Tasks(ctx, errQueue)
		if err != nil {
			t.Fatalf("Tasks(%q): %v", errQueue, err)
		}
		if len(tasks) == 1 {
			if tasks[0].ID != inputTask.ID {
				t.Errorf("quarantined task ID = %q, want %q", tasks[0].ID, inputTask.ID)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("input task was not quarantined to the error queue")
		case err := <-errCh:
			t.Fatalf("worker exited before quarantine: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	runCancel()
	if err := <-errCh; err != nil && !entroq.IsCanceled(err) {
		t.Errorf("worker exit: %v", err)
	}
}

// WorkerHoldsEmptyGroup verifies that a worker keeps a doc set it claimed
// empty for as long as its handler runs. Renewing a set's docs renews the
// set, but an empty set has none, so without its own renewal the claim
// would lapse after one lease and another claimant could take the set
// mid-work.
func WorkerHoldsEmptyGroup(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_holds_empty_group")
	ns := path.Join(qPrefix, "worker_holds_empty_group_docs")
	const lease = 300 * time.Millisecond

	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	inWork := make(chan int, 1)
	letFinish := make(chan bool)
	w := worker.New(client,
		worker.WithTakeDocs(func(context.Context, *entroq.Task, json.RawMessage) (*worker.TakeResult, error) {
			return worker.Take(entroq.ClaimKey(ns, "empty")), nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, _ json.RawMessage, sets []*entroq.DocSet) (*worker.Result, error) {
			inWork <- len(sets[0].Docs)
			<-letFinish
			return worker.Modify(task.Delete()), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, worker.Watching(queue), worker.WithLease(lease))
	}()

	select {
	case n := <-inWork:
		if n != 0 {
			t.Fatalf("Handler got %d docs from an empty set", n)
		}
	case err := <-errCh:
		t.Fatalf("Worker exited before work started: %v", err)
	case <-ctx.Done():
		t.Fatal("Timeout waiting for work to start")
	}

	// Several leases pass while the handler works; nobody else may claim the
	// set meanwhile.
	//
	// How far into the hold an intruder got through says which thing broke. A
	// win after more than one lease means a renewal was late or missing, so the
	// set really had lapsed. A win inside the first renewal interval means the
	// set was still held and the claim was allowed through anyway, which is a
	// doc-locking defect and not a timing one. Without the elapsed time the two
	// are indistinguishable, and the second is far more serious.
	workStarted := time.Now()
	for deadline := workStarted.Add(4 * lease); time.Now().Before(deadline); time.Sleep(lease / 4) {
		got, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "empty"), entroq.ClaimingSetsAs("intruder"), entroq.ClaimingSetsFor(lease))
		if err == nil {
			elapsed := time.Since(workStarted)
			// A renewal that stopped is the cause, where a lapsed set is only
			// the symptom, and the worker reports the former through errCh. It
			// is read without blocking, because a worker still renewing
			// correctly has nothing to say here and must not be waited for.
			cause := "worker still running, so renewal did not stop"
			select {
			case werr := <-errCh:
				cause = fmt.Sprintf("worker had already exited: %v", werr)
			default:
			}
			t.Fatalf("Another claimant took the worker's empty set %v into its handler (lease %v, renewal every %v, so %v of margin per cycle); %s; took %v",
				elapsed.Round(time.Millisecond), lease, entroq.RenewalDurationFor(lease), lease-entroq.RenewalDurationFor(lease), cause, got)
		}
		if !entroq.IsDependency(err) {
			t.Fatalf("Intruder claim: %v", err)
		}
	}
	letFinish <- true

	runCancel()
	if err := <-errCh; err != nil && !entroq.IsCanceled(err) {
		t.Errorf("Worker exit: %v", err)
	}
}

// WorkerReleasesSets verifies the worker's doc-claim contract: a claim is a
// transaction scoped to the handler body, so every set the body held goes back
// when the body ends, in the very modification that ends it.
//
// Only one thing keeps a set: an arrival the body puts in the FUTURE, whether
// as a set arrival or a member write asking to hold it. Depending on a doc only
// watches it. A member written with no arrival is a set the body is done with.
// And the task need not be in the modification at all -- the sets belong to the
// body, not to the task, so a body that touches only docs still frees them.
//
// Where the body commits something, the release rides along, so these subtests
// assert it the INSTANT the commit lands. Polling there would be weaker: it
// would pass just as well if the release were still a second call afterwards.
//
// Where the body commits nothing of its own, there is no commit to synchronize
// on, so those subtests must await the release -- and the deadline is half a
// lease, shared across the sets. The sets were claimed until the task's own
// arrival, so nothing can pass by merely lapsing within that bound, which is
// what keeps "released" distinct from "expired on its own". One deadline for
// all of them, not one each: waiting per key would let the total run past a
// lease, and a set freed by lapsing would then be mistaken for one released.
func WorkerReleasesSets(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	// The lease is long, so a set that is free soon after the task was
	// handled was released, not expired.
	const lease = 10 * time.Second

	// run works queue until done is closed, and returns a function that stops
	// it and reports how it exited.
	run := func(t *testing.T, queue string, opts ...worker.Option[json.RawMessage]) func() {
		t.Helper()
		w := worker.New(client, opts...)
		runCtx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() {
			errCh <- w.Run(runCtx, worker.Watching(queue), worker.WithLease(lease))
		}()
		return func() {
			cancel()
			if err := <-errCh; err != nil && !entroq.IsCanceled(err) {
				t.Errorf("Worker exit: %v", err)
			}
		}
	}
	// free reports whether an intruder can claim the set at once.
	free := func(t *testing.T, ns, key string) bool {
		t.Helper()
		_, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, key), entroq.ClaimingSetsAs("intruder"), entroq.ClaimingSetsFor(time.Second))
		if err != nil && !entroq.IsDependency(err) {
			t.Fatalf("Intruder claim of %q: %v", key, err)
		}
		return err == nil
	}
	// gone waits for the queue to empty.
	gone := func(t *testing.T, queue string) {
		t.Helper()
		for deadline := time.Now().Add(lease / 2); ; time.Sleep(20 * time.Millisecond) {
			tasks, err := client.Tasks(ctx, queue)
			if err != nil {
				t.Fatalf("Tasks: %v", err)
			}
			if len(tasks) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("Task in %q not handled in time", queue)
			}
		}
	}

	t.Run("a commit releases the sets it did not hold into the future", func(t *testing.T) {
		queue := path.Join(qPrefix, "worker_releases_commit")
		ns := path.Join(qPrefix, "worker_releases_commit_docs")
		if _, err := client.Modify(ctx,
			entroq.InsertingInto(queue),
			entroq.PuttingDocInto(ns, entroq.WithKeys("written", "")),
			entroq.PuttingDocInto(ns, entroq.WithKeys("depended", "")),
		); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		stop := run(t, queue,
			worker.WithTakeDocs(func(context.Context, *entroq.Task, json.RawMessage) (*worker.TakeResult, error) {
				return worker.Take(entroq.ClaimKey(ns, "written"), entroq.ClaimKey(ns, "depended"), entroq.ClaimKey(ns, "empty")), nil
			}),
			worker.WithDoModify(func(_ context.Context, task *entroq.Task, _ json.RawMessage, sets []*entroq.DocSet) (*worker.Result, error) {
				written, depended := sets[2].Docs[0], sets[0].Docs[0] // sorted by key
				return worker.Modify(
					task.Delete(),
					written.Change(entroq.WithContent("done"), entroq.WithDocArrivalTimeBy(lease)),
					depended.Depend(),
				), nil
			}),
		)
		defer stop()
		gone(t, queue)

		// Three things are in play, and two of them are freed BY the commit:
		//
		//   - "depended" is named in Depends. Watching a doc says nothing
		//     about wanting to keep its set, so it is released.
		//   - "empty" is not in the modify at all, so nothing decided its
		//     arrival and it is released.
		//   - "written" is changed with an arrival a lease away. That is
		//     explicit intent to keep holding it, and the only thing that
		//     keeps a set.
		//
		// Asserted with no polling. The releases are in the same transaction
		// as the task's deletion, so the moment the task is gone they have
		// landed; awaiting them here would pass even if they had not.
		for _, key := range []string{"depended", "empty"} {
			if !free(t, ns, key) {
				t.Errorf("A set the commit did not hold into the future (%q): want it released by the commit itself", key)
			}
		}
		if free(t, ns, "written") {
			t.Error("A set the commit held into the future: want it to keep the arrival the commit gave it")
		}
	})

	t.Run("a retry releases the sets", func(t *testing.T) {
		queue := path.Join(qPrefix, "worker_releases_retry")
		ns := path.Join(qPrefix, "worker_releases_retry_docs")
		if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		retried := make(chan bool, 1)
		stop := run(t, queue,
			worker.WithTakeDocs(func(context.Context, *entroq.Task, json.RawMessage) (*worker.TakeResult, error) {
				return worker.Take(entroq.ClaimKey(ns, "held")), nil
			}),
			worker.WithDoModify(func(context.Context, *entroq.Task, json.RawMessage, []*entroq.DocSet) (*worker.Result, error) {
				select {
				case retried <- true:
				default:
				}
				return nil, worker.RetryErrorf("not yet").After(time.Hour)
			}),
		)
		defer stop()
		<-retried
		// The retry's own modification carries the release, so the set is free
		// the moment the retry has landed -- no window in which the task could
		// be claimed again while its docs were still held. The retry is visible
		// as the attempt it counted; a retried task keeps its claimant and
		// simply arrives later, so neither of those says it has landed.
		for deadline := time.Now().Add(lease / 2); ; time.Sleep(20 * time.Millisecond) {
			tasks, err := client.Tasks(ctx, queue)
			if err != nil {
				t.Fatalf("Tasks: %v", err)
			}
			if len(tasks) > 0 && tasks[0].Attempt > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Retry did not land in time")
			}
		}
		if !free(t, ns, "held") {
			t.Error("A set held by a retried task: want it released by the retry itself")
		}
	})

	t.Run("a body that commits nothing still releases its sets", func(t *testing.T) {
		queue := path.Join(qPrefix, "worker_releases_nothing")
		ns := path.Join(qPrefix, "worker_releases_nothing_docs")
		if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		done := make(chan bool, 1)
		stop := run(t, queue,
			worker.WithTakeDocs(func(context.Context, *entroq.Task, json.RawMessage) (*worker.TakeResult, error) {
				return worker.Take(entroq.ClaimKey(ns, "held"), entroq.ClaimKey(ns, "empty")), nil
			}),
			worker.WithDoModify(func(context.Context, *entroq.Task, json.RawMessage, []*entroq.DocSet) (*worker.Result, error) {
				select {
				case done <- true:
				default:
				}
				// Nothing to commit. The body still ended, so the sets still
				// go back -- in a modification of their own, since there is no
				// other. The task is left alone and keeps its lease.
				return nil, nil
			}),
		)
		defer stop()
		<-done
		// Awaited, not immediate: nothing was committed, so there is no
		// observable event to synchronize on. One deadline across both sets.
		deadline := time.Now().Add(lease / 2)
		for _, key := range []string{"held", "empty"} {
			for !free(t, ns, key) {
				if time.Now().After(deadline) {
					t.Errorf("A set held by a body that committed nothing (%q): want it released anyway", key)
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})

	t.Run("a body that leaves the task alone still releases its sets", func(t *testing.T) {
		queue := path.Join(qPrefix, "worker_releases_docs_only")
		ns := path.Join(qPrefix, "worker_releases_docs_only_docs")
		if _, err := client.Modify(ctx,
			entroq.InsertingInto(queue),
			entroq.PuttingDocInto(ns, entroq.WithKeys("written", "")),
		); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		done := make(chan bool, 1)
		stop := run(t, queue,
			worker.WithTakeDocs(func(context.Context, *entroq.Task, json.RawMessage) (*worker.TakeResult, error) {
				return worker.Take(entroq.ClaimKey(ns, "written")), nil
			}),
			worker.WithDoModify(func(_ context.Context, _ *entroq.Task, _ json.RawMessage, sets []*entroq.DocSet) (*worker.Result, error) {
				select {
				case done <- true:
				default:
				}
				// Writes a doc and says nothing about the task, so the task
				// keeps its lease. The set is still the body's to give back,
				// and the write asks for no future arrival.
				return worker.Modify(sets[0].Docs[0].Change(entroq.WithContent("done"))), nil
			}),
		)
		defer stop()
		<-done
		for deadline := time.Now().Add(lease / 2); !free(t, ns, "written"); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("A set written by a body that left the task alone: want it released anyway")
			}
		}
	})

	t.Run("a release makes the task and its sets ready now", func(t *testing.T) {
		queue := path.Join(qPrefix, "worker_releases_release")
		ns := path.Join(qPrefix, "worker_releases_release_docs")
		if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		calls := make(chan time.Time, 2)
		first := true
		stop := run(t, queue,
			worker.WithTakeDocs(func(context.Context, *entroq.Task, json.RawMessage) (*worker.TakeResult, error) {
				return worker.Take(entroq.ClaimKey(ns, "held")), nil
			}),
			worker.WithDoModify(func(_ context.Context, task *entroq.Task, _ json.RawMessage, sets []*entroq.DocSet) (*worker.Result, error) {
				calls <- time.Now()
				if first {
					first = false
					return worker.Modify(entroq.Arriving(entroq.ReadyNow().Tasks(task).Docs(sets...))), nil
				}
				return worker.Modify(task.Delete()), nil
			}),
		)
		defer stop()
		released := <-calls
		// The worker claims the task and its set again at once: both were
		// ready, not left to their leases.
		select {
		case again := <-calls:
			if d := again.Sub(released); d > lease/2 {
				t.Errorf("Released task claimed again after %v, want well within its lease %v", d, lease)
			}
		case <-time.After(lease / 2):
			t.Fatal("Released task not claimed again within half its lease")
		}
		gone(t, queue)
	})
}

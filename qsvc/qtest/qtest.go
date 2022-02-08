// Package qtest contains standard testing routines for exercising various backends in similar ways.
package qtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqgrpc"
	"github.com/shiblon/entroq/qsvc"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/shiblon/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const bufSize = 1 << 20

// Dialer returns a net connection.
type Dialer func() (net.Conn, error)

// Tester runs a test helper, all of which these test functions are.
type Tester func(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string)

// ClientService starts an in-memory gRPC network service via StartService,
// then creates an EntroQ client that connects to it. It returns the client and
// a function that can be deferred for cleanup.
//
// The opener is used by the service to connect to storage. The client always
// uses a grpc opener.
func ClientService(ctx context.Context, opener entroq.BackendOpener) (client *entroq.EntroQ, stop func(), err error) {
	s, dial, err := StartService(ctx, opener)
	if err != nil {
		return nil, nil, fmt.Errorf("client service: %w", err)
	}
	defer func() {
		if err != nil {
			s.Stop()
		}
	}()

	client, err = entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(dial),
		eqgrpc.WithInsecure()))
	if err != nil {
		return nil, nil, fmt.Errorf("start client on in-memory service: %w", err)
	}

	return client, func() {
		client.Close()
		s.Stop()
	}, nil
}

// StartService starts an in-memory gRPC network service and returns a function for creating client connections to it.
func StartService(ctx context.Context, opener entroq.BackendOpener) (*grpc.Server, Dialer, error) {
	lis := bufconn.Listen(bufSize)
	svc, err := qsvc.New(ctx, opener)
	if err != nil {
		return nil, nil, fmt.Errorf("start service: %w", err)
	}
	s := grpc.NewServer()
	hpb.RegisterHealthServer(s, health.NewServer())
	pb.RegisterEntroQServer(s, svc)
	go s.Serve(lis)

	return s, lis.Dial, nil
}

// SimpleChange tests that changing things in the task leave most of it intact, and can handle things like queue moves.
func SimpleChange(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	inQueue := path.Join(qPrefix, "simple_change", "in")
	outQueue := path.Join(qPrefix, "simple_change", "out")

	inserted, _, err := client.Modify(ctx, entroq.InsertingInto(inQueue))
	if err != nil {
		t.Fatalf("Error inserting: %v", err)
	}
	_, changed, err := client.Modify(ctx, inserted[0].AsChange(entroq.QueueTo(outQueue)))
	if err != nil {
		t.Fatalf("Error changing: %v", err)
	}
	if changed[0].Queue != outQueue {
		t.Fatalf("Change queue: want %q, got %v", outQueue, changed[0].Queue)
	}
	changed[0].Queue = inQueue

	if diff := EqualTasksVersionIncr(inserted[0], changed[0], 1); diff != "" {
		t.Fatalf("Tasks not equal (except version bump):\n%v", diff)
	}
}

// SimpleWorker tests basic worker functionality while tasks are coming in and
// being waited on.
func SimpleWorker(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "simple_worker")

	var consumed []*entroq.Task
	ctx, cancel := context.WithCancel(ctx)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return client.NewWorker(queue).Run(ctx, func(ctx context.Context, task *entroq.Task) ([]entroq.ModifyArg, error) {
			if task.Claims != 1 {
				return nil, fmt.Errorf("worker claim expected claims to be 1, got %d", task.Claims)
			}
			consumed = append(consumed, task)
			return []entroq.ModifyArg{task.AsDeletion()}, nil
		})
	})

	select {
	case <-time.After(1 * time.Second):
	case <-ctx.Done():
		t.Fatalf("Sleep: %v", ctx.Err())
	}

	var inserted []*entroq.Task
	for i := 0; i < 10; i++ {
		ins, _, err := client.Modify(ctx, entroq.InsertingInto(queue))
		if err != nil {
			t.Fatalf("Failed to insert task: %v", err)
		}
		inserted = append(inserted, ins...)
		select {
		case <-ctx.Done():
			t.Fatalf("Canceled while inserting: %v", ctx.Err())
		default:
		}
	}

	for {
		empty, err := client.QueuesEmpty(ctx, entroq.MatchExact(queue))
		if err != nil {
			t.Fatalf("Error checking for empty queue: %v", err)
		}
		if empty {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Context error waiting for queues to empty: %v", err)
		default:
		}
	}

	cancel()
	if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
		t.Fatalf("Worker exit error: %v", err)
	}

	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, consumed, expectVersionIncr(1)); diff != "" {
		t.Errorf("Tasks inserted not the same as tasks consumed:\n%v", diff)
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
			entroq.InsertingInto(bigQueue, entroq.WithValue([]byte("big value"))),
		}
		if i < medSize {
			args = append(args, entroq.ModifyArg(
				entroq.InsertingInto(medQueue, entroq.WithValue([]byte("med value"))),
			))
		}
		if i < smallSize {
			args = append(args, entroq.ModifyArg(
				entroq.InsertingInto(smallQueue, entroq.WithValue([]byte("smallvalue"))),
			))
		}
		if _, _, err := client.Modify(ctx, args...); err != nil {
			t.Fatalf("Insert queues failed: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Keep track of what was consumed when.
	var consumed []*entroq.Task
	consumedCh := make(chan *entroq.Task)

	g, ctx := errgroup.WithContext(ctx)

	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			ti := 0
			w := client.NewWorker(bigQueue, medQueue, smallQueue)
			err := w.Run(ctx, func(ctx context.Context, task *entroq.Task) ([]entroq.ModifyArg, error) {
				ti++
				if task.Claims != 1 {
					return nil, fmt.Errorf("worker claim expected to be 1, was %d", task.Claims)
				}
				consumedCh <- task
				return []entroq.ModifyArg{task.AsDeletion()}, nil
			})
			if entroq.IsCanceled(err) {
				return nil
			}
			return err
		})
	}

	g.Go(func() error {
		waitCtx, _ := context.WithTimeout(ctx, 1*time.Minute)
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
		t.Fatalf("Error in worker")
	}

	// Now check that we consumed the right tasks from the right queues..
	queuesFound := make(map[string]int)
	var smallIndices []int
	var medIndices []int

	for i, t := range consumed {
		queuesFound[t.Queue]++
		switch t.Queue {
		case medQueue:
			medIndices = append(medIndices, i)
		case smallQueue:
			smallIndices = append(smallIndices, i)
		}
	}

	// assume sorted
	sortedMedian := func(indices []int) float64 {
		if len(indices)%2 == 1 {
			return float64(indices[len(indices)/2])
		}
		return float64(indices[len(indices)/2-1]+indices[len(indices)/2]) / 2
	}

	smallMedian := sortedMedian(smallIndices)
	medMedian := sortedMedian(medIndices)

	if found := queuesFound[bigQueue]; found != bigSize {
		t.Errorf("Expected to consume %d from big queue, consumed %d", bigSize, found)
	}
	if found := queuesFound[medQueue]; found != medSize {
		t.Errorf("Expected to consume %d from med queue, consumed %d", medSize, found)
	}
	if found := queuesFound[smallQueue]; found != smallSize {
		t.Errorf("Expected to consume %d from small queue, consumed %d", smallSize, found)
	}

	const (
		maxExpectedSmallMedian = float64(smallSize*3/2 + smallSize)
		maxExpectedMedMedian   = float64((medSize-smallSize)*2+smallSize*3)/2 + medSize
	)

	if smallMedian > maxExpectedSmallMedian {
		t.Errorf("Expected small median to max out at around %f, but was %f", maxExpectedSmallMedian, smallMedian)
	}

	if medMedian > maxExpectedMedMedian {
		t.Errorf("Expected med median to max out at around %f, but was %f", maxExpectedMedMedian, medMedian)
	}
}

// WorkerDependencyHandler tests that workers with specified dependency
// handlers get called on dependency errors, and that upgrades to fatal errors
// happen appropriately.
func WorkerDependencyHandler(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "dep-handler-queue")

	timesHandled := make(chan int, 1)
	timesHandled <- 0

	upgradeError := errors.New("upgraded")

	leaseTime := 3 * time.Second

	onDep := func(err entroq.DependencyError) error {
		h := <-timesHandled
		defer func() {
			timesHandled <- h + 1
			// Also give the task time to expire before the loop tries again,
			// otherwise it waits for a long time (no notification for tasks
			// becoming available).
			time.Sleep(leaseTime)
		}()
		switch h {
		case 0:
			return nil // first time through, we ignore it, don't upgrade it.
		case 1:
			return upgradeError // second time through upgrade it.
		default:
			t.Fatalf("Called dependency handler too many times: %v", timesHandled)
			return nil
		}
	}

	if _, _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Failed to insert task: %v", err)
	}

	w := client.NewWorker(queue).WithOpts(entroq.WithDependencyHandler(onDep), entroq.WithLease(leaseTime))

	ctx, cancel := context.WithTimeout(ctx, 3*leaseTime)
	defer cancel()

	err := w.Run(ctx, func(ctx context.Context, task *entroq.Task) ([]entroq.ModifyArg, error) {
		// Return a modification that will fail because it depends on a non-existent task ID.
		return []entroq.ModifyArg{task.AsDeletion(), entroq.DependingOn(uuid.New(), 0, entroq.WithIDQueue("no queue"))}, nil
	})

	if !errors.Is(err, upgradeError) {
		t.Fatalf("Expected upgrade error, got %v", err)
	}

	if h := <-timesHandled; h != 2 {
		t.Fatalf("Expected to have a safe error, then a fatal error, only ran %v times", h)
	}
}

// WorkerRetryOnError test that workers that have RetryTaskError results
// increment attempts and set the error properly. It also checks that after max
// attempts, things get moved.
func WorkerRetryOnError(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	newTask := func(val string) *entroq.Task {
		return &entroq.Task{
			Queue: path.Join(qPrefix, "retry_on_error", val),
			ID:    uuid.New(),
			Value: []byte(val),
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
			wantErr:     "retry error: retry",
			wantMove:    false,
		},
		{
			name:        "retry-larger-max",
			input:       newTask("with max"),
			maxAttempt:  3,
			wantAttempt: 1,
			wantErr:     "retry error: with max",
			wantMove:    false,
		},
		{
			name:        "retry-too-many",
			input:       newTask("too many"),
			maxAttempt:  1,
			wantAttempt: 1,
			wantErr:     "retry error: too many",
			wantMove:    true,
		},
	}

	runWorkerOneCase := func(ctx context.Context, c tc) {
		t.Helper()

		w := client.NewWorker(c.input.Queue).WithOpts(entroq.WithBaseRetryDelay(0), entroq.WithMaxAttempts(c.maxAttempt))

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Keep track of the retried task - after we get past attempt 0, we
		// store it here. Then, if the task was not meant to be moved, we can
		// see what happened without a separate claim section.
		retriedTaskCh := make(chan *entroq.Task, 1)

		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return w.Run(gctx, func(ctx context.Context, task *entroq.Task) ([]entroq.ModifyArg, error) {
				// Only attempt this again if it's the first time.
				if task.Attempt == 0 {
					return nil, entroq.RetryTaskErrorf("retry error: %s", string(task.Value))
				}
				// Save it so we know what happened with the retry error, and clean up.
				retriedTaskCh <- task
				return []entroq.ModifyArg{task.AsDeletion()}, nil
			})
		})

		// Now stick our task in. The worker is ready and waiting.
		if _, _, err := client.Modify(ctx, entroq.InsertingInto(c.input.Queue, entroq.WithID(c.input.ID), entroq.WithValue(c.input.Value))); err != nil {
			t.Fatalf("Test %q insert task: %v", c.name, err)
		}
		waitCtx, _ := context.WithTimeout(ctx, 5*time.Second)
		var changedTask *entroq.Task
		if c.wantMove {
			// Expect the queue to become empty, get stuff out of the error queue.
			if err := client.WaitQueuesEmpty(waitCtx, entroq.MatchExact(c.input.Queue)); err != nil && !entroq.IsCanceled(err) {
				t.Fatalf("Test %q expected queue %q to become empty, didn't happen: %v", c.name, c.input.Queue, err)
			}
			// Now check that it's in the error queue and looks okay.
			errTasks, err := client.Tasks(ctx, w.ErrQMap(c.input.Queue))
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
		id := uuid.New()
		return &entroq.Task{
			Queue: path.Join(baseQueue, val, id.String()),
			ID:    id,
			Value: []byte(val),
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

	runWorkerOneCase := func(ctx context.Context, c tc) {
		// Before starting, create a brand new ID for the task in the case.
		// Otherwise we'll try to reinsert a moved task when we create work.
		c.input.ID = uuid.New()

		const leaseTime = 5 * time.Second

		w := client.NewWorker(c.input.Queue).WithOpts(
			entroq.WithLease(leaseTime),
		)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			if err := w.Run(gctx, func(ctx context.Context, task *entroq.Task) ([]entroq.ModifyArg, error) {
				switch string(task.Value) {
				case "die":
					return nil, fmt.Errorf("task asked to die")
				case "move":
					return nil, entroq.MoveTaskErrorf("task asked to move")
				case "move-wait":
					select {
					case <-time.After(leaseTime):
						return nil, entroq.MoveTaskErrorf("task asked to move after renewal")
					case <-ctx.Done():
						return nil, fmt.Errorf("oops - test %q took too long, gave up before finishing: %w", c.name, ctx.Err())
					}
				default:
					return []entroq.ModifyArg{task.AsDeletion()}, nil
				}
			}); err != nil && !entroq.IsCanceled(err) {
				// Log quickly so we can see it before waits fail below.
				log.Printf("Worker Run error: %v", err)
				return err
			}
			return nil
		})

		if _, _, err := client.Modify(ctx, entroq.InsertingInto(c.input.Queue,
			entroq.WithID(c.input.ID),
			entroq.WithValue(c.input.Value),
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
			if _, _, err := client.Modify(ctx, c.input.AsDeletion()); err != nil {
				t.Fatalf("Test %q tried to clean up dead task: %v", c.name, err)
			}
			return
		}

		waitCtx, _ := context.WithTimeout(ctx, 3*leaseTime)
		if err := client.WaitQueuesEmpty(waitCtx, entroq.MatchExact(c.input.Queue)); err != nil && !entroq.IsCanceled(err) {
			t.Fatalf("Test %q: no moved tasks found, task was not expected to die: %v", c.name, err)
		}
		errTasks, err := client.Tasks(ctx, w.ErrQMap(c.input.Queue))
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
			t.Errorf("Test %q expected task to be moved, but is not found in %q", c.name, w.ErrQMap(c.input.Queue))
		} else if !c.moved && foundTask != nil {
			t.Errorf("Test %q expected task to be deleted, but showed up in %q", c.name, w.ErrQMap(c.input.Queue))
		}

		cancel()

		err = g.Wait()
		if c.moved && err != nil && !entroq.IsCanceled(err) {
			t.Errorf("Test %q expected no error on move, got %v", c.name, err)
		} else if !c.moved && entroq.IsCanceled(err) {
			t.Errorf("Test %q expected error from worker, got none", c.name)
		}
	}

	stressCount := 30
	if testing.Short() {
		stressCount = 1
	}

	// Feed test cases one at a time to the worker, wait for empty, then
	// depending on desired outcomes, check error queue for expected value.
	for _, test := range cases {
		for i := 0; i < stressCount; i++ {
			t.Run(fmt.Sprintf("case=%v-%v", test.name, i), func(*testing.T) {
				runWorkerOneCase(ctx, test)
			})
		}
	}
}

// WorkerRenewal tests that task claims are renewed periodically for longer-running work tasks.
func WorkerRenewal(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "worker_renewal")

	_, _, err := client.Modify(ctx, entroq.InsertingInto(queue))
	if err != nil {
		t.Fatalf("Error inserting: %v", err)
	}

	// Newly-inserted task will have version 0.

	task, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(6*time.Second))
	if err != nil {
		t.Fatalf("Failed to claim task: %v", err)
	}

	// Task now has version 1.

	renewed, err := client.DoWithRenew(ctx, task, 6*time.Second, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker do with renew: %w", ctx.Err())
		case <-time.After(10 * time.Second): // long enough for 3 renewals.
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Error renewing and waiting: %v", err)
	}
	if want, got := task.Version+3, renewed.Version; want != got {
		t.Fatalf("Expected renewed task to be at version %d, got %d", want, got)
	}
}

// TasksOmitValue exercises the task query where values are not desired.
func TasksOmitValue(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "tasks_omit_value")

	inserted, _, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithValue([]byte("t1"))),
		entroq.InsertingInto(queue, entroq.WithValue([]byte("t2"))),
		entroq.InsertingInto(queue, entroq.WithValue([]byte("t3"))),
	)
	if err != nil {
		t.Fatalf("Failed to insert tasks: %v", err)
	}

	tasks, err := client.Tasks(ctx, queue, entroq.OmitValues())
	if err != nil {
		t.Fatalf("Failed to get tasks without values: %v", err)
	}
	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, tasks, expectEmptyValue()); diff != "" {
		t.Errorf("Task listing without values had unexpected results (-want +got):\n%v", diff)
	}

	tasksWithVals, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Failed to get tasks with values: %v", err)
	}
	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, tasksWithVals); diff != "" {
		t.Fatalf("Task listing with values had unexpected results (-want +got):\n%v", diff)
	}
}

// TasksWithID exercises the task query mechanism that allows specific task IDs to be looked up.
func TasksWithID(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "tasks_with_id")

	ids := []uuid.UUID{
		uuid.New(),
		uuid.New(),
		uuid.New(),
		uuid.New(),
	}

	var args []entroq.ModifyArg
	for _, id := range ids {
		args = append(args, entroq.InsertingInto(queue, entroq.WithID(id)))
	}
	inserted, _, err := client.Modify(ctx, args...)
	if err != nil {
		t.Fatalf("Insertion failed: %v", err)
	}
	if want, got := len(ids), len(inserted); want != got {
		t.Fatalf("Expected %d tasks inserted, got %d", want, got)
	}
	for i, task := range inserted {
		if want, got := ids[i], task.ID; want != got {
			t.Fatalf("Inserted task should have ID %q, but has %q", want, got)
		}
	}

	// Once inserted, we should be able to query for zero (all), one, or more of them.

	// Check that no ID spec produces the right number of them.
	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Error getting tasks from queue %q: %v", queue, err)
	}
	if want, got := len(ids), len(tasks); want != got {
		t.Fatalf("Expected %d tasks in 'all' query, got %d", want, got)
	}
	want := make(map[uuid.UUID]bool)
	for _, id := range ids {
		want[id] = true
	}
	for _, task := range tasks {
		if !want[task.ID] {
			t.Fatalf("Wanted queried task %d to have ID present in task listing, but not found", task.ID)
		}
	}

	// Check that specifing a couple of the IDs works.
	idSubSet := []uuid.UUID{ids[1], ids[3]}
	tasks, err = client.Tasks(ctx, queue, entroq.WithTaskID(idSubSet...))
	if err != nil {
		t.Fatalf("Error getting tasks from queue %q: %v", queue, err)
	}
	if want, got := len(idSubSet), len(tasks); want != got {
		t.Fatalf("Expected %d tasks in 'all' query, got %d", want, got)
	}
	for i, task := range tasks {
		if want, got := idSubSet[i], task.ID; want != got {
			t.Fatalf("Wanted queried task %d to have ID %q, got %q", i, want, got)
		}
	}
}

// TasksWithIDOnly tests that tasks listed by ID only (no queue) can return from multiple queues.
func TasksWithIDOnly(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	q1 := path.Join(qPrefix, "id_only_1")
	q2 := path.Join(qPrefix, "id_only_2")

	var modArgs []entroq.ModifyArg
	for i := 0; i < 5; i++ {
		q := q1
		if i%2 == 0 {
			q = q2
		}
		val := []byte(fmt.Sprintf("val %d", i))
		modArgs = append(modArgs, entroq.InsertingInto(q, entroq.WithValue(val)))
	}

	ins, _, err := client.Modify(ctx, modArgs...)
	if err != nil {
		t.Fatalf("Initial insert failed: %v", err)
	}

	var ids1, ids2 []uuid.UUID
	var tasks1, tasks2 []*entroq.Task
	for i, t := range ins {
		if i < 3 {
			tasks1 = append(tasks1, t)
			ids1 = append(ids1, t.ID)
		} else {
			tasks2 = append(tasks2, t)
			ids2 = append(ids2, t.ID)
		}
	}

	results1, err := client.Tasks(ctx, "", entroq.WithTaskID(ids1...))
	if err != nil {
		t.Errorf("First group of task IDs had an error: %v", err)
	}
	for i, task := range results1 {
		if want, got := ids1[i], task.ID; want != got {
			t.Errorf("Expected task %d from group 1 to have ID %v, got %v", i, want, got)
		}
		if want, got := string(tasks1[i].Value), string(task.Value); want != got {
			t.Errorf("Expected task %d from group 1 to have bytes %s, got %s", i, want, got)
		}
	}

	results2, err := client.Tasks(ctx, "", entroq.WithTaskID(ids2...))
	if err != nil {
		t.Errorf("First group of task IDs had an error: %v", err)
	}
	for i, task := range results2 {
		if want, got := ids2[i], task.ID; want != got {
			t.Errorf("Expected task %d from group 2 to have ID %v, got %v", i, want, got)
		}
		if want, got := string(tasks2[i].Value), string(task.Value); want != got {
			t.Errorf("Expected task %d from group 2 to have bytes %s, got %s", i, want, got)
		}
	}
}

// InsertWithID tests the ability to insert tasks with a specified ID,
// including errors when an existing ID is used for insertion.
func InsertWithID(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "insert_with_id")

	knownID := uuid.New()

	// Insert task with an explicit ID.
	inserted, changed, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithID(knownID)))
	if err != nil {
		t.Fatalf("Unable to insert task with known ID %q: %v", knownID, err)
	}

	// Check that insertion with explicit IDs works.
	if len(changed) != 0 {
		t.Fatalf("Expected 0 changed tasks, got %d", len(changed))
	}
	if len(inserted) != 1 {
		t.Fatalf("Expected 1 inserted task, got %d", len(inserted))
	}

	insertedTask := inserted[0]
	if insertedTask.ID != knownID {
		t.Fatalf("Expected inserted task to have ID %q, got %q", knownID, insertedTask.ID)
	}

	// Try to claim the just-inserted task.
	claimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Unexpected error claiming task with ID %q: %v", knownID, err)
	}
	if claimed == nil {
		t.Fatalf("Expected task from queue %q, but received none", queue)
	}
	if claimed.ID != knownID {
		t.Fatalf("Task claim expected ID %q, got %q", knownID, claimed.ID)
	}

	// Try to insert with a known ID that's already there.
	_, _, err = client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithID(knownID)))
	if err == nil {
		t.Fatalf("Expected error inserting with existing ID %v, but got no error", knownID)
	}
	depErr, ok := entroq.AsDependency(err)
	if !ok {
		t.Fatalf("Expected dependency error, got %v", err)
	}
	if want, got := 1, len(depErr.Inserts); want != got {
		t.Fatalf("Expected %d insertion errors in dependency error, got %v", want, got)
	}

	// Try to insert with a known ID when the task is in a different queue.
	_, _, err = client.Modify(ctx, entroq.InsertingInto(queue+"/elsewhere", entroq.WithID(knownID)))
	if err == nil {
		t.Fatalf("Expected error inserting existing ID %v into a different queue, but got no error", knownID)
	}
	depErr, ok = entroq.AsDependency(err)
	if !ok {
		t.Fatalf("Expected dependency error, got %v", err)
	}
	if want, got := 1, len(depErr.Inserts); want != got {
		t.Fatalf("Expected %d insertion errors in dependency error, got %v", want, got)
	}

	// Try to insert again, but allow it to be skipped.
	_, _, err = client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithID(knownID), entroq.WithSkipColliding(true)))
	if err != nil {
		t.Fatalf("Expected no error inserting with existing skippable ID %v: %v", knownID, err)
	}

	// Insert another task.
	inserted, _, err = client.Modify(ctx, entroq.InsertingInto(queue))
	if err != nil {
		t.Fatalf("Expected no insertion error, got: %v", err)
	}

	// Try to insert the known ID and delete the new ID at the same time. This
	// should work when it's set to skip colliding.
	if _, _, err = client.Modify(ctx,
		entroq.InsertingInto(queue,
			entroq.WithID(knownID),
			entroq.WithSkipColliding(true)),
		inserted[0].AsDeletion()); err != nil {
		t.Fatalf("Expected no error inserting skippable and deleting, got: %v", err)
	}

	// Check that we have only one task in the queue, and that it's the expected one.
	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Error getting tasks: %v", err)
	}
	if want, got := 1, len(tasks); want != got {
		t.Fatalf("Expected len(tasks) = %d, got %v", want, got)
	}
	if want, got := knownID, tasks[0].ID; want != got {
		t.Fatalf("Expected ID %v found, got %v", want, got)
	}
}

// SimpleSequence tests some basic functionality of a task manager, over gRPC.
func SimpleSequence(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	now := time.Now()

	queue := path.Join(qPrefix, "simple_sequence")

	// Claim from empty queue.
	task, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Got unexpected error claiming from empty queue: %v", err)
	}
	if task != nil {
		t.Fatalf("Got unexpected non-nil claim response from empty queue:\n%s", task)
	}

	const futureTaskDuration = 2 * time.Second
	futureTime := now.Add(futureTaskDuration)

	insWant := []*entroq.Task{
		{
			Queue:    queue,
			At:       now,
			Value:    []byte("hello"),
			Claimant: client.ID(),
		},
		{
			Queue:    queue,
			At:       futureTime,
			Value:    []byte("there"),
			Claimant: client.ID(),
		},
	}
	var insData []*entroq.TaskData
	for _, task := range insWant {
		insData = append(insData, task.Data())
	}

	inserted, changed, err := client.Modify(ctx, entroq.Inserting(insData...))
	if err != nil {
		t.Fatalf("Got unexpected error inserting two tasks: %+v", err)
	}
	if changed != nil {
		t.Fatalf("Got unexpected changes during insertion: %+v", err)
	}
	if diff := EqualAllTasksOrderedSkipIDAndTime(insWant, inserted); diff != "" {
		t.Fatalf("Modify tasks unexpected result, ignoring ID and time fields (-want +got):\n%v", diff)
	}
	// Also check that their arrival times are 100 ms apart as expected:
	if diff := inserted[1].At.Sub(inserted[0].At); diff != futureTaskDuration {
		t.Fatalf("Wanted At difference to be %v, got %v", futureTaskDuration, diff)
	}

	// Get queues.
	queuesWant := map[string]int{queue: 2}
	queuesGot, err := client.Queues(ctx, entroq.MatchPrefix(qPrefix))
	if err != nil {
		t.Fatalf("Getting queues failed: %v", err)
	}
	if diff := cmp.Diff(queuesWant, queuesGot); diff != "" {
		t.Fatalf("Queues (-want +got):\n%v", diff)
	}

	// Get all tasks.
	tasksGot, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Tasks call failed after insertions: %v", err)
	}
	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, tasksGot); diff != "" {
		t.Fatalf("Tasks unexpected return, ignoring ID and time fields (-want +got):\n%+v", diff)
	}

	// Claim ready task.
	claimCtx, _ := context.WithTimeout(ctx, 5*time.Second)
	claimed, err := client.Claim(claimCtx, entroq.From(queue), entroq.ClaimFor(10*time.Second))

	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %+v", err)
	}
	if claimed == nil {
		t.Fatalf("Unexpected nil result from blocking Claim")
	}
	if diff := EqualTasksVersionIncr(inserted[0], claimed, 1); diff != "" {
		t.Fatalf("Claim tasks differ, ignoring ID and times:\n%v", diff)
	}
	if claimed.Claims != 1 {
		t.Fatalf("Expected claim to increment task claims to %d, got %d", 1, claimed.Claims)
	}

	// TryClaim not ready task.
	tryclaimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with no ready tasks: %v", err)
	}
	if tryclaimed != nil {
		t.Fatalf("Got unexpected non-nil claim response from a queue with no ready tasks:\n%s", tryclaimed)
	}

	// Make sure the next claim will work.
	claimCtx, cancel := context.WithTimeout(ctx, 2*futureTaskDuration)
	defer cancel()
	claimed, err = client.Claim(claimCtx,
		entroq.From(queue),
		entroq.ClaimFor(5*time.Second),
		entroq.ClaimPollTime(time.Second))
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %v", err)
	}
	if diff := EqualTasksVersionIncr(inserted[1], claimed, 1); diff != "" {
		t.Fatalf("Claim got unexpected task, ignoring ID and time fields (-want +got):\n%v", diff)
	}
	log.Printf("Now: %v", now)
	if got := claimed.At; got.Before(futureTime) {
		t.Fatalf("Claimed arrival time %v came earlier than expedcted time %v", got, futureTime)
	}
	if claimed.Claims != 1 {
		t.Fatalf("Expected claim to increment task claims to %d, got %d", 1, claimed.Claims)
	}
}

// QueueMatch tests various queue matching functions against a client.
func QueueMatch(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue1 := path.Join(qPrefix, "queue-1")
	queue2 := path.Join(qPrefix, "queue-2")
	queue3 := path.Join(qPrefix, "queue-3")
	quirkyQueue := path.Join(qPrefix, "quirky=queue")

	wantQueues := map[string]int{
		queue1:      1,
		queue2:      2,
		queue3:      3,
		quirkyQueue: 1,
	}

	// Add tasks so that queues have a certain number of things in them, as above.
	var toInsert []entroq.ModifyArg
	for q, n := range wantQueues {
		for i := 0; i < n; i++ {
			toInsert = append(toInsert, entroq.InsertingInto(q))
		}
	}
	inserted, _, err := client.Modify(ctx, toInsert...)
	if err != nil {
		t.Fatalf("in QueueMatch - inserting empty tasks: %v", err)
	}

	// Check that we got everything inserted.
	if want, got := len(inserted), len(toInsert); want != got {
		t.Fatalf("in QueueMatch - want %d inserted, got %d", want, got)
	}

	// Check that we can get exact numbers for all of the above using MatchExact.
	for q, n := range wantQueues {
		qs, err := client.Queues(ctx, entroq.MatchExact(q))
		if err != nil {
			t.Fatalf("QueueMatch single - getting queue: %v", err)
		}
		if len(qs) != 1 {
			t.Errorf("QueueMatch single - expected 1 entry, got %d", len(qs))
		}
		if want, got := n, qs[q]; want != got {
			t.Errorf("QueueMatch single - expected %d values in queue %q, got %d", want, q, got)
		}
	}

	// Check that passing multiple exact matches works properly.
	multiExactCases := []struct {
		q1 string
		q2 string
	}{
		{queue1, queue2},
		{queue1, queue3},
		{quirkyQueue, queue2},
		{"bogus", queue3},
	}

	for _, c := range multiExactCases {
		qs, err := client.Queues(ctx, entroq.MatchExact(c.q1), entroq.MatchExact(c.q2))
		if err != nil {
			t.Fatalf("QueueMatch multi - getting multiple queues: %v", err)
		}
		if len(qs) > 2 {
			t.Errorf("QueueMatch multi - expected no more than 2 entries, got %d", len(qs))
		}
		want1, want2 := wantQueues[c.q1], wantQueues[c.q2]
		if got1, got2 := qs[c.q1], qs[c.q2]; want1 != got1 || want2 != got2 {
			t.Errorf("QueueMatch multi - wanted %q:%d, %q:%d, got %q:%d, %q:%d", c.q1, want1, c.q2, want2, c.q1, got1, c.q2, got2)
		}
	}

	// Check prefix matching.
	prefixCases := []struct {
		prefix string
		qn     int
		n      int
	}{
		{path.Join(qPrefix, "queue-"), 3, 6},
		{path.Join(qPrefix, "qu"), 4, 7},
		{path.Join(qPrefix, "qui"), 1, 1},
	}

	for _, c := range prefixCases {
		qs, err := client.Queues(ctx, entroq.MatchPrefix(c.prefix))
		if err != nil {
			t.Fatalf("QueueMatch prefix - queues error: %v", err)
		}
		if want, got := c.qn, len(qs); want != got {
			t.Errorf("QueueMatch prefix - want %d queues, got %d", want, got)
		}
		tot := 0
		for _, n := range qs {
			tot += n
		}
		if want, got := c.n, tot; want != got {
			t.Errorf("QueueMatch prefix - want %d total items, got %d", want, got)
		}
	}
}

// QueueStats checks that queue stats basically work.
func QueueStats(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	nothingClaimedQueue := path.Join(qPrefix, "queue-1")
	partiallyClaimedQueue := path.Join(qPrefix, "queue-2")

	if _, _, err := client.Modify(ctx,
		entroq.InsertingInto(nothingClaimedQueue),
		entroq.InsertingInto(nothingClaimedQueue),
		entroq.InsertingInto(partiallyClaimedQueue),
		entroq.InsertingInto(partiallyClaimedQueue),
		entroq.InsertingInto(partiallyClaimedQueue),
	); err != nil {
		t.Fatalf("Insert into queues: %v", err)
	}

	// Now claim something.
	if _, err := client.Claim(ctx, entroq.From(partiallyClaimedQueue)); err != nil {
		t.Fatalf("Couldn't claim: %v", err)
	}

	got, err := client.QueueStats(ctx, entroq.MatchPrefix(qPrefix))
	if err != nil {
		t.Fatalf("Queue stats error: %v", err)
	}

	want := map[string]*entroq.QueueStat{
		nothingClaimedQueue: {
			Name:      nothingClaimedQueue,
			Size:      2,
			Claimed:   0,
			Available: 2,
			MaxClaims: 0,
		},
		partiallyClaimedQueue: {
			Name:      partiallyClaimedQueue,
			Size:      3,
			Claimed:   1,
			Available: 2,
			MaxClaims: 1,
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("QueueStats (-want +got):\n%v", diff)
	}
}

type taskQueueVersionValue struct {
	Queue   string
	Version int32
	Value   []byte
}

func newTaskQueueVersionValue(t *entroq.Task) *taskQueueVersionValue {
	n := &taskQueueVersionValue{
		Queue:   t.Queue,
		Version: t.Version,
		Value:   make([]byte, len(t.Value)),
	}
	copy(n.Value, t.Value)
	return n
}

type taskIDQueueVersionValue struct {
	Queue    string
	ID       uuid.UUID
	Version  int32
	Value    []byte
	Claimant uuid.UUID
}

func newTaskIDQueueVersionValue(t *entroq.Task) *taskIDQueueVersionValue {
	n := &taskIDQueueVersionValue{
		ID:       t.ID,
		Queue:    t.Queue,
		Version:  t.Version,
		Claimant: t.Claimant,
		Value:    make([]byte, len(t.Value)),
	}
	copy(n.Value, t.Value)
	return n
}

type cmpOpts struct {
	versionIncr int
	valueEmpty  bool
}

type cmpOpt func(*cmpOpts)

func expectVersionIncr(by int) cmpOpt {
	if by < 0 {
		by = 0
	}
	return func(o *cmpOpts) {
		o.versionIncr = by
	}
}

func expectEmptyValue() cmpOpt {
	return func(o *cmpOpts) {
		o.valueEmpty = true
	}
}

// EqualAllTasksUnorderedSkipTimesAndCounters returns a diff if any non-time
// fields are different between unordered task lists. Sorts by ID to accomplish
// this.
func EqualAllTasksUnorderedSkipTimesAndCounters(want, got []*entroq.Task, opts ...cmpOpt) string {
	options := new(cmpOpts)
	for _, o := range opts {
		o(options)
	}
	ws := make([]*taskIDQueueVersionValue, len(want))
	for i, t := range want {
		ws[i] = newTaskIDQueueVersionValue(t)
		if options.versionIncr != 0 {
			ws[i].Version += int32(options.versionIncr)
		}
		if options.valueEmpty {
			ws[i].Value = []byte{}
		}
	}
	gs := make([]*taskIDQueueVersionValue, len(got))
	for i, t := range got {
		gs[i] = newTaskIDQueueVersionValue(t)
	}

	sort.Slice(ws, func(i, j int) bool {
		return ws[i].ID.String() < ws[j].ID.String()
	})

	sort.Slice(gs, func(i, j int) bool {
		return gs[i].ID.String() < gs[j].ID.String()
	})

	return cmp.Diff(ws, gs)
}

// EqualAllTasksUnorderedByValue checks that all task values from want are represented in got.
// It assumes IDs and queues are not important. Suitable for tests that set a
// unique value per task, but don't have ID information. Returns a string diff if different.
func EqualAllTasksUnorderedByValue(want, got []*entroq.Task) string {
	wantVals := make([][]byte, len(want))
	for i, w := range want {
		wantVals[i] = make([]byte, len(w.Value))
		copy(wantVals[i], w.Value)
	}

	gotVals := make([][]byte, len(got))
	for i, w := range got {
		gotVals[i] = make([]byte, len(w.Value))
		copy(gotVals[i], w.Value)
	}

	sort.Slice(wantVals, func(a, b int) bool {
		return bytes.Compare(wantVals[a], wantVals[b]) < 0
	})
	sort.Slice(gotVals, func(a, b int) bool {
		return bytes.Compare(gotVals[a], gotVals[b]) < 0
	})
	return cmp.Diff(wantVals, gotVals)
}

// EqualAllTasksOrderedSkipIDAndTime returns a string diff if the order of
// tasks or values of tasks in two slices is not equal. The slices should also be
// the same length.
func EqualAllTasksOrderedSkipIDAndTime(want, got []*entroq.Task) string {
	gotSimplified := make([]*taskQueueVersionValue, len(got))
	for i, t := range got {
		gotSimplified[i] = newTaskQueueVersionValue(t)
	}

	wantSimplified := make([]*taskQueueVersionValue, len(want))
	for i, t := range want {
		wantSimplified[i] = newTaskQueueVersionValue(t)
	}

	return cmp.Diff(wantSimplified, gotSimplified)
}

// EqualTasksVersionIncr checks for equality, allowing a version increment.
func EqualTasksVersionIncr(want, got *entroq.Task, versionBump int) string {
	return EqualAllTasksUnorderedSkipTimesAndCounters([]*entroq.Task{want}, []*entroq.Task{got}, expectVersionIncr(versionBump))
}

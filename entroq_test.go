package entroq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

func Example() {
	// Create an in-memory EntroQ instance.
	// Other backends are available, notably eqpg to use Postgres, or eqgrpc to
	// speak to EntroQ as a GRPC client. In the GRPC case, see cmd/eq*svc to
	// start up the server side of a GRPC connection.
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("Can't open eq: %v", err)
	}
	defer eq.Close()

	// Queues appear and disappear as tasks are added or removed. They are
	// essentially an ephemeral concept. Names are just text.
	// Task values are JSON and can be any valid JSON value.

	// Insert a few tasks:
	resp, err := eq.Modify(ctx,
		entroq.InsertingInto("q1", entroq.WithValue("hello")),
		entroq.InsertingInto("q2", entroq.WithValue("hello")))
	if err != nil {
		log.Fatalf("Error inserting tasks: %v", err)
	}

	// You can get auto-assigned IDs, versions, etc. from each inserted task:
	for _, t := range resp.InsertedTasks {
		log.Printf("Task inserted: %s\n", t.ID)
	}

	// Fire up a worker to read them. Workers can listen on one or more queues
	// simultaneously. If more than one queue is specified, it will attempt to
	// read from them uniformly randomly for fairness. This can be used to
	// "wedge in" tasks that need to be handled quickly while larger batches are
	// going elsewhere.
	w := worker.New(eq,
		// Workers claim a task and pass it to your handler functions. In the
		// background, the task's lease is renewed while the first function runs.
		worker.WithDoWork(func(ctx context.Context, initial *entroq.Task, v string, _ []*entroq.Doc) error {
			fmt.Printf("Worker handling task %q\n", v)
			// Do work with it here.
			return nil
		}),
		// When ready to commit changes to the task (including deletion), the second
		// function passes the version-stable task after the renewer is stopped,
		// making it safe to use it in modification transactions.
		worker.WithFinish(func(ctx context.Context, final *entroq.Task, v string, _ []*entroq.Doc) error {
			fmt.Printf("Deleting task %q\n", v)
			_, err := eq.Modify(ctx, final.Delete())
			if err != nil {
				return err
			}
			return nil
		}),
	)

	// The worker runs forever, so for the sake of this example we cancel it
	// pretty quickly. Other errors typically just cause a retry.
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	if err := w.Run(ctx, worker.Watching("q1", "q2")); err != nil {
		log.Fatal(err)
	}

	// Output:
	// Worker handling task "hello"
	// Deleting task "hello"
	// Worker handling task "hello"
	// Deleting task "hello"
}

func Example_dependencies() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("failed to open memory backend: %v", err)
	}
	defer eq.Close()

	// 1. Insert a configuration task and a worker task.
	_, err = eq.Modify(ctx,
		entroq.InsertingInto("config", entroq.WithRawValue(json.RawMessage(`{"max_retries": 5}`))), // []byte() also works
		entroq.InsertingInto("worker", entroq.WithValue("do work")),
	)
	if err != nil {
		log.Fatalf("insert failed: %v", err)
	}

	// 2. We use a worker that depends on the config task. If the config is modified
	// while the worker is processing, the worker's commit will fail, and it will retry.
	var config *entroq.Task

	w := worker.New(eq,
		worker.WithDoWork(func(ctx context.Context, initial *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
			if config == nil {
				tasks, err := eq.Tasks(ctx, "config")
				if err != nil || len(tasks) == 0 {
					return fmt.Errorf("failed to load config")
				}
				config = tasks[0]
			}
			// ... do work with initial and config ...
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, final *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) error {
			if config == nil {
				return fmt.Errorf("config missing during finalize")
			}

			_, err := eq.Modify(ctx, final.Delete(), config.Depend())
			if err != nil {
				if depErr, ok := entroq.AsDependency(err); ok {
					if len(depErr.Depends) > 0 {
						// It was our config task that caused the failed commit.
						// Something changed it out from under us, so we can't
						// trust that the work we did was done with the right
						// config. We could just fall off the end and the task will
						// be picked up again after its arrival time expires.
						// Or we could force a retry and increment it's attempts
						// (return RetryError). But there's nothing wrong with the task
						// so far as we know, so make it immediately available.
						if _, err := eq.Modify(ctx, final.Change(entroq.ArrivalTimeBy(0))); err != nil {
							// NOW it's our task that's the problem. Just bail.
							return fmt.Errorf("task reset after config change: %w", err)
						}
					}
				}
				return fmt.Errorf("commit failed: %w", err)
			}
			return nil
		}),
	)

	// For the sake of the example: cancel the worker after 2 seconds.
	// You won't actually do this in production.
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	if err := w.Run(ctx, worker.Watching("worker")); err != nil && !entroq.IsCanceled(err) {
		log.Fatalf("worker failed: %v", err)
	}

	tasks, err := eq.Tasks(context.Background(), "worker")
	if err != nil {
		log.Fatalf("list tasks: %v", err)
	}
	fmt.Printf("tasks remaining: %d\n", len(tasks))

	// Output:
	// tasks remaining: 0
}

func Example_manualClaimAndRenew() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatal(err)
	}
	defer eq.Close()

	// While eq.NewWorker is recommended for general use, you can manually
	// claim and renew tasks for custom daemon implementations.

	// Insert a task to work on.
	if _, err := eq.Modify(ctx, entroq.InsertingInto("manual_queue", entroq.WithValue("work"))); err != nil {
		log.Fatalf("insert failed: %v", err)
	}

	// 1. Manually claim the task.
	task, err := eq.Claim(ctx, entroq.From("manual_queue"), entroq.ClaimFor(5*time.Second))
	if err != nil {
		log.Fatalf("claim failed: %v", err)
	}

	// 2. Wrap your work in worker.DoWithRenew so the task doesn't expire while you work.
	err = worker.DoWithRenew(ctx, eq, task, 5*time.Second, func(ctx context.Context, stop worker.FinalizeRenew) error {

		// ... do some long running work ...

		// 3. Stop background renewal to get a stable, finalized version of the task.
		finalTask := stop()

		// 4. Commit the work (by deleting the task or mutating it).
		if _, err := eq.Modify(ctx, finalTask.Delete()); err != nil {
			return fmt.Errorf("failed to commit: %w", err)
		}
		return nil
	})

	if err != nil {
		log.Fatalf("manual processing failed: %v", err)
	}
	fmt.Println("task processed")

	// Output:
	// task processed
}

func TestDoWithRenewAll_ImmediateCancellationOnLeaseLoss(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("failed to create eq: %v", err)
	}
	defer eq.Close()

	queue := "/test/cancel"
	_, err = eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("work")))
	if err != nil {
		t.Fatalf("failed to insert: %v", err)
	}
	// 1. Claim the task.
	claimed, err := eq.Claim(ctx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("failed to claim: %v", err)
	}

	// 2. Start renewing.
	errChan := make(chan error, 1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	go func() {
		// Use a short renewal interval to speed up the test.
		errChan <- worker.DoWithRenewAll(ctx, eq, []*entroq.Task{claimed}, 100*time.Millisecond, func(ctx context.Context, stop worker.FinalizeRenewAll) error {
			// Wait for the context to be canceled from the outside.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	// 3. Poach the task from underneath the worker.
	// Bump version by claiming or deleting. Same claimant, so no need to spoof.
	if _, err := eq.Modify(ctx, claimed.Delete()); err != nil {
		t.Fatalf("failed to steal task: %v", err)
	}

	// 4. Verify that DoWithRenewAll returns DependencyError (wrapped or direct)
	// and that it happened quickly.
	select {
	case err := <-errChan:
		if _, ok := entroq.AsDependency(err); !ok {
			t.Errorf("expected DependencyError, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("timed out waiting for worker cancellation after lease loss")
	}
}

// TestDependencyErrorDocFields verifies that a DependencyError produced by a
// missing-doc Modify contains populated doc fields and that Error() mentions them.
func TestDependencyErrorDocFields(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open eq: %v", err)
	}
	defer eq.Close()

	ns := "test/dep-error-docs"
	claimant := "test-claimant"

	// Insert a real doc so we have a valid version to depend on.
	resp, err := eq.Modify(ctx, entroq.ModifyAs(claimant), entroq.CreatingIn(ns,
		entroq.WithIDKeys("real-doc", "", ""),
		entroq.WithContent(json.RawMessage(`"present"`)),
	))
	if err != nil {
		t.Fatalf("insert doc: %v", err)
	}
	realDoc := resp.InsertedDocs[0]

	// Attempt a Modify that depends on a nonexistent doc.
	_, err = eq.Modify(ctx, entroq.ModifyAs(claimant),
		entroq.DependingOnDocID(ns, "ghost-doc", 0),
		// Also depend on real doc at wrong version to trigger missing-change path.
		realDoc.Change(entroq.WithContent(json.RawMessage(`"updated"`))),
	)
	if err == nil {
		t.Fatal("expected dependency error, got nil")
	}
	depErr, ok := entroq.AsDependency(err)
	if !ok {
		t.Fatalf("expected *DependencyError, got %T: %v", err, err)
	}

	// DocDepends must identify the missing doc.
	if len(depErr.DocDepends) == 0 {
		t.Errorf("DependencyError.DocDepends is empty; want ghost-doc listed")
	}
	// Error() string must mention the doc namespace/ID.
	msg := depErr.Error()
	if !containsAll(msg, ns, "ghost-doc") {
		t.Errorf("DependencyError.Error() does not mention missing doc; got:\n%s", msg)
	}
}

// TestDependencyErrorDocCopy verifies that DependencyError.Copy preserves doc fields.
func TestDependencyErrorDocCopy(t *testing.T) {
	did := &entroq.DocID{Namespace: "ns", ID: "doc-1", Version: 3}
	orig := &entroq.DependencyError{
		DocDeletes: []*entroq.DocID{did},
		DocClaims:  []*entroq.DocID{did},
	}
	cp := orig.Copy()
	if len(cp.DocDeletes) != 1 {
		t.Errorf("Copy() lost DocDeletes: got %d, want 1", len(cp.DocDeletes))
	}
	if len(cp.DocClaims) != 1 {
		t.Errorf("Copy() lost DocClaims: got %d, want 1", len(cp.DocClaims))
	}
}

// TestOnlyClaimsIgnoresMissingDocs verifies that OnlyClaims returns false when
// docs are missing, not just when tasks are missing.
func TestOnlyClaimsIgnoresMissingDocs(t *testing.T) {
	depErr := &entroq.DependencyError{
		DocDeletes: []*entroq.DocID{{Namespace: "ns", ID: "d1", Version: 1}},
	}
	if depErr.OnlyClaims() {
		t.Error("OnlyClaims() returned true with missing DocDeletes; want false")
	}
}

// Example_docBasics demonstrates creating, reading, updating, and deleting a doc.
func Example_docBasics() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Create a doc in a namespace. Docs are indexed by primary and optional
	// secondary key. The ID is assigned automatically; use the key to address
	// the doc in queries and claims. Use WithIDKeys only when you need an
	// explicit ID (e.g. journal replay or cross-service references).
	resp, err := eq.Modify(ctx,
		entroq.CreatingIn("config",
			entroq.WithKeys("server", ""),
			entroq.WithContent(map[string]any{"port": 8080}),
		),
	)
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	doc := resp.InsertedDocs[0]
	fmt.Printf("created doc key=%s version %d\n", doc.Key, doc.Version)

	// Read it back.
	docs, err := eq.Docs(ctx, entroq.DocsIn("config"))
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	fmt.Printf("listed %d doc(s)\n", len(docs))

	// Update the content. The doc's current version acts as an optimistic lock.
	resp, err = eq.Modify(ctx,
		doc.Change(entroq.WithContent(map[string]any{"port": 9090})),
	)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	updated := resp.ChangedDocs[0]
	fmt.Printf("updated doc key=%s to version %d\n", updated.Key, updated.Version)

	// Delete it.
	if _, err := eq.Modify(ctx, entroq.DeletingDoc(updated)); err != nil {
		log.Fatalf("delete: %v", err)
	}

	docs, err = eq.Docs(ctx, entroq.DocsIn("config"))
	if err != nil {
		log.Fatalf("list after delete: %v", err)
	}
	fmt.Printf("listed %d doc(s) after delete\n", len(docs))

	// Output:
	// created doc key=server version 1
	// listed 1 doc(s)
	// updated doc key=server to version 2
	// listed 0 doc(s) after delete
}

// Example_docKeyRange demonstrates listing docs by primary-key range.
func Example_docKeyRange() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	ns := "metrics"
	// Insert docs whose primary keys are sortable timestamps.
	for _, k := range []string{"2024-01-01", "2024-06-15", "2025-03-10"} {
		if _, err := eq.Modify(ctx,
			entroq.CreatingIn(ns,
				entroq.WithKeys(k, ""),
				entroq.WithContent(k),
			),
		); err != nil {
			log.Fatalf("insert %s: %v", k, err)
		}
	}

	// List docs in the half-open range [2024-06-15, 2025-01-01).
	docs, err := eq.Docs(ctx, entroq.DocsIn(ns).WithKeyRange("2024-06-15", "2025-01-01"))
	if err != nil {
		log.Fatalf("range query: %v", err)
	}
	for _, d := range docs {
		fmt.Println(d.Key)
	}

	// Output:
	// 2024-06-15
}

// Example_docAtomicTaskCommit demonstrates claiming a doc and deleting a task
// in a single atomic Modify, so the doc update and task completion are inseparable.
func Example_docAtomicTaskCommit() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Create a shared state doc and a work task.
	resp, err := eq.Modify(ctx,
		entroq.CreatingIn("state",
			entroq.WithKeys("counter", ""),
			entroq.WithContent(0),
		),
		entroq.InsertingInto("jobs", entroq.WithValue("increment")),
	)
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
	stateDoc := resp.InsertedDocs[0]

	// Claim the work task.
	task, err := eq.Claim(ctx, entroq.From("jobs"), entroq.ClaimFor(5*time.Second))
	if err != nil {
		log.Fatalf("claim task: %v", err)
	}

	// Claim the state doc for exclusive modification.
	claimed, err := eq.ClaimDocs(ctx, entroq.ClaimKey("state", stateDoc.Key).For(5*time.Second))
	if err != nil {
		log.Fatalf("claim doc: %v", err)
	}
	counter := claimed[0]

	var count int
	if err := json.Unmarshal(counter.Content, &count); err != nil {
		log.Fatalf("unmarshal: %v", err)
	}
	count++

	// Atomically: update the counter doc and delete the task.
	// If either the task or doc version has changed since we claimed it,
	// the whole Modify fails -- nothing is committed.
	if _, err := eq.Modify(ctx,
		task.Delete(),
		counter.Change(entroq.WithContent(count)),
	); err != nil {
		log.Fatalf("atomic commit: %v", err)
	}

	// Verify the final state.
	docs, err := eq.Docs(ctx, entroq.DocsIn("state"))
	if err != nil {
		log.Fatalf("read state: %v", err)
	}
	var final int
	if err := json.Unmarshal(docs[0].Content, &final); err != nil {
		log.Fatalf("unmarshal final: %v", err)
	}
	fmt.Printf("counter = %d\n", final)

	tasks, err := eq.Tasks(ctx, "jobs")
	if err != nil {
		log.Fatalf("list jobs: %v", err)
	}
	fmt.Printf("jobs remaining = %d\n", len(tasks))

	// Output:
	// counter = 1
	// jobs remaining = 0
}

// Example_docClaimContention demonstrates the all-or-nothing locking behavior
// of ClaimDocs. All docs sharing a primary key are claimed together; a second
// claimant attempting the same key while the lock is held gets a DependencyError.
// A key that does not exist returns an empty slice rather than an error.
func Example_docClaimContention() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	const ns = "locks"
	const key = "resource"

	// A missing key returns an empty slice, not an error.
	empty, err := eq.ClaimDocs(ctx, entroq.ClaimKey(ns, "no-such-key").For(time.Second))
	if err != nil {
		log.Fatalf("missing-key claim: %v", err)
	}
	fmt.Printf("missing key: %d docs\n", len(empty))

	// Create two docs sharing the same primary key.
	if _, err := eq.Modify(ctx,
		entroq.CreatingIn(ns, entroq.WithKeys(key, "shard-0"), entroq.WithContent(0)),
		entroq.CreatingIn(ns, entroq.WithKeys(key, "shard-1"), entroq.WithContent(0)),
	); err != nil {
		log.Fatalf("create: %v", err)
	}

	// First claimant acquires both docs atomically.
	docs, err := eq.ClaimDocs(ctx, entroq.ClaimKey(ns, key))
	if err != nil {
		log.Fatalf("first claim: %v", err)
	}
	fmt.Printf("first claim: %d docs\n", len(docs))

	// A second claimant is blocked while the first holds the lock.
	// The lock expires automatically after Duration; use doc.Change() to
	// release it early (which increments the version).
	_, err = eq.ClaimDocs(ctx, &entroq.DocClaim{
		Namespace: ns,
		Claimant:  "other-claimant",
		Key:       key,
		Duration:  entroq.DefaultClaimDuration,
	})
	fmt.Printf("contention: IsDependency=%v\n", entroq.IsDependency(err))

	// Output:
	// missing key: 0 docs
	// first claim: 2 docs
	// contention: IsDependency=true
}

// Example_docVersionPin demonstrates DependingOnDoc as an optimistic-lock
// check. The dependency pins a Modify to a specific doc version without
// modifying the doc: the operation succeeds only when the doc is still at the
// expected version. HasMissingDocs is true when any version-pinned dependency
// fails -- whether the doc is genuinely absent or simply at a different version.
func Example_docVersionPin() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Create a config doc.
	resp, err := eq.Modify(ctx,
		entroq.CreatingIn("config",
			entroq.WithKeys("limits", ""),
			entroq.WithContent(map[string]any{"max": 100}),
		),
	)
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	v1 := resp.InsertedDocs[0]
	fmt.Printf("created at version %d\n", v1.Version)

	// Update the doc so v1 is now stale.
	resp, err = eq.Modify(ctx,
		v1.Change(entroq.WithContent(map[string]any{"max": 200})),
	)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	v2 := resp.ChangedDocs[0]
	fmt.Printf("updated to version %d\n", v2.Version)

	// Depend on the old version -- fails because the doc has moved on.
	_, err = eq.Modify(ctx,
		entroq.InsertingInto("work", entroq.WithValue("job")),
		v1.Depend(),
	)
	if de, ok := entroq.AsDependency(err); ok {
		fmt.Printf("stale pin: IsDependency=true, HasMissingDocs=%v\n", de.HasMissingDocs())
	}

	// Depend on the current version -- succeeds.
	if _, err = eq.Modify(ctx,
		entroq.InsertingInto("work", entroq.WithValue("job")),
		v2.Depend(),
	); err != nil {
		log.Fatalf("current pin: %v", err)
	}
	fmt.Println("current pin: ok")

	// Output:
	// created at version 1
	// updated to version 2
	// stale pin: IsDependency=true, HasMissingDocs=true
	// current pin: ok
}

// Example_docQueryByID demonstrates fetching specific docs by explicit ID.
// When IDs are specified, key-range filtering and Limit are ignored and docs
// are returned in the order the IDs are listed.
func Example_docQueryByID() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Create docs with known IDs for stable querying.
	if _, err := eq.Modify(ctx,
		entroq.CreatingIn("items",
			entroq.WithIDKeys("id-alpha", "item", "0"),
			entroq.WithContent("first"),
		),
		entroq.CreatingIn("items",
			entroq.WithIDKeys("id-beta", "item", "1"),
			entroq.WithContent("second"),
		),
		entroq.CreatingIn("items",
			entroq.WithIDKeys("id-gamma", "item", "2"),
			entroq.WithContent("third"),
		),
	); err != nil {
		log.Fatalf("create: %v", err)
	}

	// Fetch a specific subset by ID; order follows the IDs slice.
	docs, err := eq.Docs(ctx, entroq.DocsIn("items").WithIDs("id-gamma", "id-alpha"))
	if err != nil {
		log.Fatalf("query by ID: %v", err)
	}
	for _, d := range docs {
		var s string
		if err := d.ContentAs(&s); err != nil {
			log.Fatalf("unmarshal: %v", err)
		}
		fmt.Printf("id=%s content=%s\n", d.ID, s)
	}

	// Output:
	// id=id-gamma content=third
	// id=id-alpha content=first
}

// containsAll returns true if s contains all of the given substrings.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

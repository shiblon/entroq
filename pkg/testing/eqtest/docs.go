package eqtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
	"golang.org/x/sync/errgroup"
)

// InitialVersions verifies that newly inserted tasks and docs both begin at
// version zero. Their first changes must therefore advance them to version one.
func InitialVersions(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "initial_versions", "tasks")
	namespace := path.Join(qPrefix, "initial_versions", "docs")

	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue,
			entroq.WithID(uniqueTaskID("task-1")),
			entroq.WithValue("task"),
		),
		entroq.PuttingDocInto(namespace,
			entroq.WithIDKeys("doc-1", "", ""),
			entroq.WithContent("doc"),
		),
	)
	if err != nil {
		t.Fatalf("Insert task and doc: %v", err)
	}
	if len(resp.InsertedTasks) != 1 {
		t.Fatalf("InsertedTasks length: want 1, got %d", len(resp.InsertedTasks))
	}
	if len(resp.InsertedDocs) != 1 {
		t.Fatalf("InsertedDocs length: want 1, got %d", len(resp.InsertedDocs))
	}
	if got := resp.InsertedTasks[0].Version; got != 0 {
		t.Errorf("Initial task version: want 0, got %d", got)
	}
	if got := resp.InsertedDocs[0].Version; got != 0 {
		t.Errorf("Initial doc version: want 0, got %d", got)
	}

	resp, err = client.Modify(ctx,
		resp.InsertedTasks[0].Change(entroq.ValueTo("changed task")),
		resp.InsertedDocs[0].Change(entroq.WithContent("changed doc")),
	)
	if err != nil {
		t.Fatalf("Change task and doc: %v", err)
	}
	if len(resp.ChangedTasks) != 1 {
		t.Fatalf("ChangedTasks length: want 1, got %d", len(resp.ChangedTasks))
	}
	if len(resp.ChangedDocs) != 1 {
		t.Fatalf("ChangedDocs length: want 1, got %d", len(resp.ChangedDocs))
	}
	if got := resp.ChangedTasks[0].Version; got != 1 {
		t.Errorf("First changed task version: want 1, got %d", got)
	}
	if got := resp.ChangedDocs[0].Version; got != 1 {
		t.Errorf("First changed doc version: want 1, got %d", got)
	}
}

// SimpleDocLifecycle tests basic insertion, change, and deletion of a doc.
func SimpleDocLifecycle(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "simple_doc")
	id := "doc-1"
	val := json.RawMessage(`{"foo":"bar"}`)

	// Insert a doc
	resp, err := client.Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithIDKeys(id, "", ""),
		entroq.WithContent(val),
	))
	if err != nil {
		t.Fatalf("Insert doc: %v", err)
	}
	if len(resp.InsertedDocs) != 1 {
		t.Fatalf("InsertedDocs length: want 1, got %v", len(resp.InsertedDocs))
	}
	res := resp.InsertedDocs[0]
	if res.ID != id || res.Namespace != ns {
		t.Errorf("Inserted doc metadata mismatch: got %v/%v, want %v/%v", res.Namespace, res.ID, ns, id)
	}

	// Change the doc
	newVal := json.RawMessage(`{"foo":"baz"}`)
	resp, err = client.Modify(ctx, res.Change(entroq.WithContent(newVal)))
	if err != nil {
		t.Fatalf("Change doc: %v", err)
	}
	if len(resp.ChangedDocs) != 1 {
		t.Fatalf("ChangedDocs length: want 1, got %v", len(resp.ChangedDocs))
	}
	changed := resp.ChangedDocs[0]
	// Compare semantically: PostgreSQL normalizes JSONB whitespace on retrieval.
	var wantJ, gotJ any
	if err := json.Unmarshal(newVal, &wantJ); err != nil {
		t.Fatalf("Unmarshal want: %v", err)
	}
	if err := json.Unmarshal(changed.Content, &gotJ); err != nil {
		t.Fatalf("Unmarshal got: %v", err)
	}
	if diff := cmp.Diff(wantJ, gotJ); diff != "" {
		t.Errorf("Changed content mismatch (-want +got):\n%s", diff)
	}
	if changed.Version != res.Version+1 {
		t.Errorf("Changed version: want %v, got %v", res.Version+1, changed.Version)
	}
	// requested.At = current.At (insertion time, past) → claimant must be cleared.
	if changed.Claimant != "" {
		t.Errorf("Content-only change on unclaimed doc: claimant should be empty, got %q", changed.Claimant)
	}

	// Try to depend on the doc before it changed (should fail)
	_, err = client.Modify(ctx, res.Change(entroq.WithRawContent(json.RawMessage(`{"fail": true}`))))
	if err == nil {
		t.Fatal("Expected dependency error when changing old version, got nil")
	}
	if !entroq.IsDependency(err) {
		t.Fatalf("Expected DependencyError, got %T: %v", err, err)
	}

	// Delete the doc
	_, err = client.Modify(ctx, changed.Delete())
	if err != nil {
		t.Fatalf("Delete doc: %v", err)
	}

	// Ensure that it's gone
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil {
		t.Fatalf("List docs: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("Docs after delete: want 0, got %v", len(docs))
	}
}

// DocMultiOp tests that tasks and docs can be modified together.
func DocMultiOp(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "multi_op_ns")
	q := path.Join(qPrefix, "multi_op_q")

	// Atomic insert of a task and its associated state doc.
	resp, err := client.Modify(ctx,
		entroq.InsertingInto(q, entroq.WithValue("task body")),
		entroq.PuttingDocInto(ns,
			entroq.WithIDKeys("state-1", "", ""),
			entroq.WithContent("initial state"),
		),
	)
	if err != nil {
		t.Fatalf("Multi-insert: %v", err)
	}
	task := resp.InsertedTasks[0]
	res := resp.InsertedDocs[0]

	// Atomic transition: finish task, move to 'done' queue, and update doc.
	doneQ := path.Join(q, "done")
	_, err = client.Modify(ctx,
		task.Change(entroq.QueueTo(doneQ)),
		res.Change(entroq.WithContent("finished state")),
	)
	if err != nil {
		t.Fatalf("Multi-transition: %v", err)
	}

	// Verify both changed.
	tasks, err := client.Tasks(ctx, doneQ)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("Failed to verify task move: %v, len=%v", err, len(tasks))
	}
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil || len(docs) != 1 {
		t.Fatalf("Failed to verify doc update: %v, len=%v", err, len(docs))
	}
	if string(docs[0].Content) != `"finished state"` {
		t.Errorf("Doc content mismatch: %s", docs[0].Content)
	}
}

// DocListing tests the listing and range filtering of docs.
func DocListing(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "listing_ns")

	data := []struct{ ID, PK string }{
		{"a", "1"},
		{"b", "2"},
		{"c", "3"},
		{"d", "4"},
	}

	var args []entroq.ModifyArg
	for _, d := range data {
		args = append(args, entroq.PuttingDocInto(ns,
			entroq.WithIDKeys(d.ID, d.PK, ""),
			entroq.WithContent(nil),
		))
	}

	if _, err := client.Modify(ctx, args...); err != nil {
		t.Fatalf("Setup listing: %v", err)
	}

	// List all
	res, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil || len(res) != 4 {
		t.Fatalf("List all: err=%v, len=%v", err, len(res))
	}

	// Filter by Primary Key range [2, 4) -- KeyEnd is exclusive.
	// Keys "2" and "3" qualify; "1" is below the start, "4" is excluded.
	res, err = client.Docs(ctx, &entroq.DocQuery{
		Namespace: ns,
		KeyStart:  "2",
		KeyEnd:    "4",
	})
	if err != nil {
		t.Fatalf("Filter range: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("Filter range [2,4): want 2 results, got %d", len(res))
	}
	for _, r := range res {
		if r.Key < "2" || r.Key >= "4" {
			t.Errorf("Doc PK out of range [2,4): %q", r.Key)
		}
	}

	// Filter by one complete primary key. Secondary keys and IDs are suffixes
	// in some backend indexes, so this also pins the exact-prefix boundary.
	res, err = client.Docs(ctx, &entroq.DocQuery{Namespace: ns, KeyExact: "2"})
	if err != nil {
		t.Fatalf("Filter exact: %v", err)
	}
	if len(res) != 1 || res[0].ID != "b" {
		t.Fatalf("Filter exact 2: want doc b, got %v", res)
	}
}

// DocConcurrencyStress hammers the doc locking logic by having multiple
// goroutines attempt to read-modify-write the same docs simultaneously,
// relying on version-based dependency checks for optimistic concurrency.
func DocConcurrencyStress(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "stress_ns")
	const (
		numDocs    = 10
		numWorkers = 20
		iterations = 50
	)

	// Setup: Create docs with an initial counter value of 0.
	var setupArgs []entroq.ModifyArg
	for i := range numDocs {
		setupArgs = append(setupArgs, entroq.PuttingDocInto(ns,
			entroq.WithIDKeys(fmt.Sprintf("doc-%d", i), "", ""),
			entroq.WithContent(0),
		))
	}
	if _, err := client.Modify(ctx, setupArgs...); err != nil {
		t.Fatalf("Setup stress: %v", err)
	}

	// Workers: Each worker picks a random doc and increments its counter.
	// Version-based dependency checks detect conflicts; workers retry on collision.
	// parentCtx is preserved for final verification after the errgroup context is done.
	parentCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	for range numWorkers {
		g.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			for range iterations {
				docID := fmt.Sprintf("doc-%d", rng.Intn(numDocs))
				for {
					docList, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns})
					if err != nil {
						return fmt.Errorf("get docs: %w", err)
					}
					var res *entroq.Doc
					for _, r := range docList {
						if r.ID == docID {
							res = r
							break
						}
					}
					if res == nil {
						return fmt.Errorf("doc %s not found", docID)
					}

					var val int
					if err := json.Unmarshal(res.Content, &val); err != nil {
						return fmt.Errorf("unmarshal: %w", err)
					}

					_, err = client.Modify(ctx, res.Change(entroq.WithContent(val+1)))
					if err == nil {
						break // success
					}
					if !entroq.IsDependency(err) {
						return fmt.Errorf("modify: %w", err)
					}
					// Dependency error: version conflict, retry.
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Duration(rng.Intn(10)) * time.Millisecond):
					}
				}
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		t.Fatalf("Stress test failed: %v", err)
	}

	// Final Verification: Sum of all counters must equal numWorkers * iterations.
	// Use parentCtx -- the errgroup ctx is canceled after g.Wait() returns.
	docs, err := client.Docs(parentCtx, &entroq.DocQuery{Namespace: ns})
	if err != nil {
		t.Fatalf("Final list: %v", err)
	}

	total := 0
	for _, r := range docs {
		var val int
		if err := json.Unmarshal(r.Content, &val); err != nil {
			t.Fatalf("Final unmarshal: %v", err)
		}
		total += val
	}

	if want := numWorkers * iterations; total != want {
		t.Errorf("Total count mismatch: want %d, got %d", want, total)
	}
}

// MixedAtomicStress ensures that tasks and docs can be modified together
// under high contention, verifying that combined queue+namespace locking
// remains atomic and deadlock-free.
func MixedAtomicStress(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "mixed_ns")
	q := path.Join(qPrefix, "mixed_q")
	const (
		numItems   = 10
		numWorkers = 10
		iterations = 30
	)

	// Setup: each 'item' has a task and a corresponding doc, both starting at 0.
	for i := range numItems {
		id := fmt.Sprintf("item-%d", i)
		if _, err := client.Modify(ctx,
			entroq.InsertingInto(q, entroq.WithID(id), entroq.WithValue(0)),
			entroq.PuttingDocInto(ns,
				entroq.WithIDKeys(id, "", ""),
				entroq.WithContent(0),
			),
		); err != nil {
			t.Fatalf("Setup mixed stress: %v", err)
		}
	}

	// parentCtx is preserved for final verification after the errgroup context is done.
	parentCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	for range numWorkers {
		g.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			for range iterations {
				for {
					task, err := client.TryClaim(ctx, entroq.From(q))
					if err != nil {
						return fmt.Errorf("claim task: %w", err)
					}
					if task == nil {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(time.Duration(rng.Intn(5)) * time.Millisecond):
							continue
						}
					}
					id := task.ID // ID was set at creation, matches the doc ID.

					docList, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns})
					if err != nil {
						return fmt.Errorf("list docs: %w", err)
					}
					var res *entroq.Doc
					for _, r := range docList {
						if r.ID == id {
							res = r
							break
						}
					}
					if res == nil {
						return fmt.Errorf("doc %s not found", id)
					}

					var tVal, rVal int
					if err := json.Unmarshal(task.Value, &tVal); err != nil {
						return fmt.Errorf("unmarshal task: %w", err)
					}
					if err := json.Unmarshal(res.Content, &rVal); err != nil {
						return fmt.Errorf("unmarshal doc: %w", err)
					}

					_, err = client.Modify(ctx,
						task.Change(entroq.ValueTo(tVal+1), entroq.ArrivalTimeBy(0)),
						res.Change(entroq.WithContent(rVal+1)),
					)
					if err == nil {
						break // success
					}
					if !entroq.IsDependency(err) {
						return fmt.Errorf("mixed modify: %w", err)
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Duration(rng.Intn(5)) * time.Millisecond):
					}
				}
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		t.Fatalf("Mixed stress failed: %v", err)
	}

	// Verify: both task and doc counters must total numWorkers * iterations.
	// Use parentCtx -- the errgroup ctx is canceled after g.Wait() returns.
	tasks, err := client.Tasks(parentCtx, q)
	if err != nil {
		t.Fatalf("Final tasks: %v", err)
	}
	docs, err := client.Docs(parentCtx, &entroq.DocQuery{Namespace: ns})
	if err != nil {
		t.Fatalf("Final docs: %v", err)
	}

	tSum, rSum := 0, 0
	for _, task := range tasks {
		var v int
		if err := json.Unmarshal(task.Value, &v); err != nil {
			t.Fatalf("Unmarshal task value: %v", err)
		}
		tSum += v
	}
	for _, r := range docs {
		var v int
		if err := json.Unmarshal(r.Content, &v); err != nil {
			t.Fatalf("Unmarshal doc content: %v", err)
		}
		rSum += v
	}

	expected := numWorkers * iterations
	if tSum != expected || rSum != expected {
		t.Errorf("Sum mismatch: tasks=%d, docs=%d, want=%d", tSum, rSum, expected)
	}
}

// DocClaimLocking verifies the all-or-nothing locking behavior of ClaimDocs:
// a second claimant cannot claim the same key while docs are held by the first,
// and the lock expires so a third claim can succeed after the duration elapses.
func DocClaimLocking(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "claim_lock_ns")
	const key = "shared-key"

	// Insert two docs with the same primary key.
	if _, err := client.Modify(ctx,
		entroq.PuttingDocInto(ns, entroq.WithKeys(key, "a"), entroq.WithContent(1)),
		entroq.PuttingDocInto(ns, entroq.WithKeys(key, "b"), entroq.WithContent(2)),
	); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// First claimant acquires the lock.
	const claimDur = 500 * time.Millisecond
	docs, err := client.ClaimDocs(ctx, &entroq.DocClaim{
		Namespace: ns,
		Claimant:  "claimant-A",
		Key:       key,
		Duration:  claimDur,
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("first claim: want 2 docs, got %d", len(docs))
	}

	// Second claimant must fail while first holds the lock.
	_, err = client.ClaimDocs(ctx, &entroq.DocClaim{
		Namespace: ns,
		Claimant:  "claimant-B",
		Key:       key,
		Duration:  claimDur,
	})
	if err == nil {
		t.Fatal("second claim should fail while first holds lock, got nil error")
	}
	if !entroq.IsDependency(err) {
		t.Fatalf("expected DependencyError from second claim, got %T: %v", err, err)
	}

	// After the claim duration expires, a third claimant can succeed.
	time.Sleep(claimDur + 50*time.Millisecond)
	docs2, err := client.ClaimDocs(ctx, &entroq.DocClaim{
		Namespace: ns,
		Claimant:  "claimant-C",
		Key:       key,
		Duration:  claimDur,
	})
	if err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
	if len(docs2) != 2 {
		t.Fatalf("claim after expiry: want 2 docs, got %d", len(docs2))
	}
}

// DocInsertWithID tests collision detection and skip-colliding behavior for
// doc inserts that specify an explicit ID.
//
// This test specifically exercises the bug path that existed before the fix:
//   - eqredis silently overwrote on explicit-ID collision instead of returning
//     DependencyError.DocInserts.
//   - eqpg called the task error parser for doc errors, misrouting collisions.
//   - The entroq Modify retry loop didn't handle DocInserts collisions at all.
func DocInsertWithID(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "doc_insert_with_id")
	knownID := client.GenID()

	// Insert a doc with an explicit ID -- should succeed.
	resp, err := client.Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithIDKeys(knownID, "pk", "sk"),
		entroq.WithContent("first"),
	))
	if err != nil {
		t.Fatalf("first insert with explicit ID %q: %v", knownID, err)
	}
	if len(resp.InsertedDocs) != 1 {
		t.Fatalf("first insert: want 1 InsertedDoc, got %d", len(resp.InsertedDocs))
	}
	if resp.InsertedDocs[0].ID != knownID {
		t.Fatalf("first insert: want ID %q, got %q", knownID, resp.InsertedDocs[0].ID)
	}

	// Inserting the same explicit ID again must return DependencyError with
	// DocInserts populated -- not silently overwrite, not misroute to Changes.
	_, err = client.Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithIDKeys(knownID, "pk", "sk"),
		entroq.WithContent("second"),
	))
	if err == nil {
		t.Fatal("second insert with same explicit ID: expected DependencyError, got nil")
	}
	depErr, ok := entroq.AsDependency(err)
	if !ok {
		t.Fatalf("second insert: expected DependencyError, got %T: %v", err, err)
	}
	if want, got := 1, len(depErr.DocInserts); want != got {
		t.Fatalf("second insert: want %d DocInserts in dependency error, got %d (%v)", want, got, depErr)
	}
	if depErr.DocInserts[0].ID != knownID {
		t.Fatalf("second insert: DocInserts[0].ID = %q, want %q", depErr.DocInserts[0].ID, knownID)
	}

	// Verify the original content is unchanged (no silent overwrite).
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns, IDs: []string{knownID}})
	if err != nil {
		t.Fatalf("verify after collision: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("verify after collision: want 1 doc, got %d", len(docs))
	}
	if string(docs[0].Content) != `"first"` {
		t.Errorf("verify after collision: content changed to %s, want %q", docs[0].Content, `"first"`)
	}

	// Inserting with WithSkipCollidingDoc must succeed and leave the original intact.
	_, err = client.Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithIDKeys(knownID, "pk", "sk"),
		entroq.WithContent("overwrite-attempt"),
		entroq.WithSkipCollidingDoc(true),
	))
	if err != nil {
		t.Fatalf("skip-colliding insert: expected no error, got %v", err)
	}

	docs, err = client.Docs(ctx, &entroq.DocQuery{Namespace: ns, IDs: []string{knownID}})
	if err != nil {
		t.Fatalf("verify after skip: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("verify after skip: want 1 doc, got %d", len(docs))
	}
	if string(docs[0].Content) != `"first"` {
		t.Errorf("verify after skip: content changed to %s, want %q", docs[0].Content, `"first"`)
	}

	// Skip-colliding insert alongside another real operation must execute the
	// real operation while dropping the colliding insert.
	otherID := client.GenID()
	_, err = client.Modify(ctx,
		entroq.PuttingDocInto(ns,
			entroq.WithIDKeys(knownID, "pk", "sk"),
			entroq.WithContent("overwrite-attempt"),
			entroq.WithSkipCollidingDoc(true),
		),
		entroq.PuttingDocInto(ns,
			entroq.WithIDKeys(otherID, "pk2", "sk2"),
			entroq.WithContent("new-doc"),
		),
	)
	if err != nil {
		t.Fatalf("skip-colliding + new insert: expected no error, got %v", err)
	}

	docs, err = client.Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("final list: want 2 docs, got %d", len(docs))
	}
}

// DocClaimantBehavior verifies the claimant field semantics for doc modifications.
//
// The rule: after a Modify, current.Claimant = requested.Claimant if requested.At > now, else "".
// requested.At comes from WithDocArrivalTime/WithDocArrivalTimeBy; if not supplied,
// current.At is used (the existing value is copied through).
func DocClaimantBehavior(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	ns := path.Join(qPrefix, "doc_claimant_behavior")
	insertLease := 10 * time.Second

	// A future arrival on insertion starts the document claimed by the
	// inserting client. This permits an atomic create-and-hold handoff to a
	// worker using the same client identity.
	resp, err := client.Modify(ctx, entroq.PuttingDocInto(ns,
		entroq.WithKeys("claimed-on-insert", ""),
		entroq.WithDocArrivalTimeBy(insertLease),
	))
	if err != nil {
		t.Fatalf("future-at insert: %v", err)
	}
	inserted := resp.InsertedDocs[0]
	if inserted.Claimant != client.ClientID {
		t.Errorf("after future-at insert: claimant want %q, got %q", client.ClientID, inserted.Claimant)
	}
	if time.Until(inserted.At) <= 0 {
		t.Errorf("after future-at insert: at %v is not in the future", inserted.At)
	}
	reclaimed, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "claimed-on-insert").For(insertLease))
	if err != nil {
		t.Fatalf("same-client reclaim after future-at insert: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Claimant != client.ClientID {
		t.Fatalf("same-client reclaim after future-at insert: got %+v", reclaimed)
	}

	// Insert: claimant must be empty; at defaults to now (past by query time).
	resp, err = client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("k", "")))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	doc := resp.InsertedDocs[0]
	if doc.Claimant != "" {
		t.Errorf("after insert: claimant want %q, got %q", "", doc.Claimant)
	}

	// Content-only change: requested.At = current.At (past) → claimant cleared.
	resp, err = client.Modify(ctx, doc.Change(entroq.WithContent(struct{ V int }{1})))
	if err != nil {
		t.Fatalf("content-only change: %v", err)
	}
	doc = resp.ChangedDocs[0]
	if doc.Claimant != "" {
		t.Errorf("after content-only change: claimant want %q, got %q", "", doc.Claimant)
	}

	// Change with future at: requested.At > now → claimant set to modifier.
	resp, err = client.Modify(ctx, doc.Change(entroq.WithDocArrivalTimeBy(10*time.Second)))
	if err != nil {
		t.Fatalf("change with future at: %v", err)
	}
	doc = resp.ChangedDocs[0]
	if doc.Claimant != client.ClientID {
		t.Errorf("after future-at change: claimant want %q, got %q", client.ClientID, doc.Claimant)
	}

	// Change with past at (release): requested.At <= now → claimant cleared.
	resp, err = client.Modify(ctx, doc.Change(entroq.WithDocArrivalTimeBy(-1*time.Second)))
	if err != nil {
		t.Fatalf("change with past at: %v", err)
	}
	doc = resp.ChangedDocs[0]
	if doc.Claimant != "" {
		t.Errorf("after past-at change (release): claimant want %q, got %q", "", doc.Claimant)
	}
}

// EqualDocs compares two docs for equality, allowing for a version increment.
func EqualDocs(a, b *entroq.Doc, versionDiff int32) string {
	copyA := *a
	copyA.Version += versionDiff
	// Clear time fields that are hard to compare exactly.
	copyA.At = b.At
	copyA.Created = b.Created
	copyA.Modified = b.Modified
	return cmp.Diff(&copyA, b)
}

// DocKeyRangeByteOrder verifies that doc key range queries compare keys in byte
// order, not the storage engine's locale collation, and that every backend
// agrees. Keys with punctuation are the tell: '/' (0x2F) sorts before '0'
// (0x30), so "shard/N" falls inside the half-open range [shard/, shard0), while
// "shard0" (the exclusive upper bound) does not. A locale-collated backend would
// drop the punctuated keys, so this pins byte-order semantics across backends.
func DocKeyRangeByteOrder(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "keyrange_ns")

	// "shard0" is the exclusive upper bound and must not match the range.
	for _, k := range []string{"shard/0", "shard/1", "shard/2", "shard0"} {
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns,
			entroq.WithKeys(k, ""), entroq.WithContent(nil))); err != nil {
			t.Fatalf("create doc %q: %v", k, err)
		}
	}

	res, err := client.Docs(ctx, &entroq.DocQuery{
		Namespace: ns, KeyStart: "shard/", KeyEnd: "shard0", OmitValues: true,
	})
	if err != nil {
		t.Fatalf("range query: %v", err)
	}

	got := make(map[string]bool, len(res))
	for _, d := range res {
		got[d.Key] = true
	}
	want := []string{"shard/0", "shard/1", "shard/2"}
	if len(res) != len(want) {
		var keys []string
		for _, d := range res {
			keys = append(keys, d.Key)
		}
		t.Errorf("byte-order range [shard/, shard0): got %d docs %v, want %d %v", len(res), keys, len(want), want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("byte-order range missing %q (a locale collation drops punctuated keys)", w)
		}
	}
	if got["shard0"] {
		t.Errorf("byte-order range wrongly included the exclusive upper bound %q", "shard0")
	}
}

// ModifyRejectsWrongNamespace is the doc analog of ModifyRejectsWrongQueue: a
// doc operation must name the namespace the doc actually lives in. The found map
// is keyed by (namespace, id), so naming the wrong namespace makes the doc look
// missing (a DependencyError), and a caller cannot reach a doc outside its
// authorized namespaces by naming a different one.
func ModifyRejectsWrongNamespace(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	realNS := path.Join(qPrefix, "wrong_ns_real")
	otherNS := path.Join(qPrefix, "wrong_ns_other")

	resp, err := client.Modify(ctx, entroq.PuttingDocInto(realNS,
		entroq.WithIDKeys("doc-1", "", ""),
		entroq.WithContent(json.RawMessage(`"v"`)),
	))
	if err != nil {
		t.Fatalf("insert doc: %v", err)
	}
	doc := resp.InsertedDocs[0]

	// A delete naming the wrong namespace fails as a dependency error.
	_, err = client.Modify(ctx, entroq.NewDocID(otherNS, doc.ID, doc.Version).Delete())
	if depErr, ok := entroq.AsDependency(err); !ok {
		t.Fatalf("wrong-namespace doc delete: got err %v, want a DependencyError", err)
	} else if len(depErr.DocDeletes) == 0 {
		t.Errorf("wrong-namespace doc delete: DependencyError missing a DocDeletes entry: %+v", depErr)
	}

	// A dependency naming the wrong namespace fails too.
	_, err = client.Modify(ctx, entroq.NewDocID(otherNS, doc.ID, doc.Version).Depend())
	if depErr, ok := entroq.AsDependency(err); !ok {
		t.Fatalf("wrong-namespace doc depend: got err %v, want a DependencyError", err)
	} else if len(depErr.DocDepends) == 0 {
		t.Errorf("wrong-namespace doc depend: DependencyError missing a DocDepends entry: %+v", depErr)
	}

	// A change lying about the namespace fails: the doc is not found in otherNS.
	lie := &entroq.Doc{Namespace: otherNS, ID: doc.ID, Version: doc.Version, Key: doc.Key, Content: doc.Content}
	_, err = client.Modify(ctx, lie.Change())
	if depErr, ok := entroq.AsDependency(err); !ok {
		t.Fatalf("wrong-namespace doc change: got err %v, want a DependencyError", err)
	} else if len(depErr.DocChanges) == 0 {
		t.Errorf("wrong-namespace doc change: DependencyError missing a DocChanges entry: %+v", depErr)
	}

	// The doc is untouched: still present in its real namespace at its version.
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: realNS, IDs: []string{doc.ID}})
	if err != nil {
		t.Fatalf("docs: %v", err)
	}
	if len(docs) != 1 || docs[0].Version != doc.Version {
		t.Errorf("doc should be untouched in %q at v%d: got %+v", realNS, doc.Version, docs)
	}
}

// DocTimestamps checks that a backend owns a doc's timestamps: an insert that
// does not supply them gets the current time, and a change keeps the stored
// creation time rather than taking it from the caller. Over the gRPC service
// an unset timestamp must not arrive as a real far-past date.
func DocTimestamps(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "doc_timestamps")

	before, err := client.Time(ctx)
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	// Allow for millisecond truncation on the wire.
	before = before.Add(-time.Second)

	resp, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithContent("v0")))
	if err != nil {
		t.Fatalf("Insert doc: %v", err)
	}
	inserted := resp.InsertedDocs[0]
	if inserted.Created.Before(before) || inserted.Modified.Before(before) {
		t.Fatalf("Inserted: want created and modified after %v, got created %v, modified %v", before, inserted.Created, inserted.Modified)
	}

	resp, err = client.Modify(ctx, inserted.Change(entroq.WithContent("v1")))
	if err != nil {
		t.Fatalf("Change doc: %v", err)
	}
	if got := resp.ChangedDocs[0].Created; !got.Equal(inserted.Created) {
		t.Errorf("Changed: want created %v, got %v", inserted.Created, got)
	}

	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns, IDs: []string{inserted.ID}})
	if err != nil {
		t.Fatalf("Read doc: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Want 1 stored doc, got %d", len(docs))
	}
	if got := docs[0].Created; !got.Equal(inserted.Created) {
		t.Errorf("Stored: want created %v, got %v", inserted.Created, got)
	}
}

// DocGroups checks that the docs sharing a primary key behave as one unit: a
// single version and claim cover the whole group, as a task's cover the task.
func DocGroups(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "doc_groups")
	const intruder = "intruder"
	lease := time.Minute

	readGroup := func(key string) []*entroq.Doc {
		t.Helper()
		docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns, KeyExact: key})
		if err != nil {
			t.Fatalf("Read group %q: %v", key, err)
		}
		return docs
	}

	resp, err := client.Modify(ctx,
		entroq.PuttingDocInto(ns, entroq.WithKeys("g", "a"), entroq.WithContent("a")),
		entroq.PuttingDocInto(ns, entroq.WithKeys("g", "b"), entroq.WithContent("b")),
	)
	if err != nil {
		t.Fatalf("Insert group: %v", err)
	}
	a, b := resp.InsertedDocs[0], resp.InsertedDocs[1]

	t.Run("one version per group", func(t *testing.T) {
		if a.Version != b.Version {
			t.Fatalf("Members inserted together: versions %d and %d differ", a.Version, b.Version)
		}
		if _, err := client.Modify(ctx, a.Change(entroq.WithContent("a1"))); err != nil {
			t.Fatalf("Change a: %v", err)
		}
		for _, d := range readGroup("g") {
			if d.Version == b.Version {
				t.Errorf("Member %q still at version %d after another member changed", d.ID, d.Version)
			}
		}
		// b was not touched, but the group moved, so the old version is stale.
		if _, err := client.Modify(ctx, b.Delete()); !entroq.IsDependency(err) {
			t.Errorf("Delete at the group's old version: want a dependency error, got %v", err)
		}
	})

	t.Run("a claim makes earlier reads stale", func(t *testing.T) {
		before := readGroup("g")
		held, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "g").For(lease))
		if err != nil || len(held) != 2 {
			t.Fatalf("Claim: %v, %d docs", err, len(held))
		}
		if held[0].Version == before[0].Version {
			t.Errorf("Claim did not move the group version (%d)", held[0].Version)
		}
		// Release without touching before[0]: commit a change to the other member.
		if _, err := client.Modify(ctx, held[1].Change(entroq.WithContent("released"))); err != nil {
			t.Fatalf("Holder commit: %v", err)
		}
		if _, err := client.Modify(ctx, before[0].Delete(), entroq.ModifyAs(intruder)); !entroq.IsDependency(err) {
			t.Errorf("Delete from a read taken before the claim: want a dependency error, got %v", err)
		}
	})

	t.Run("a held group refuses other writers", func(t *testing.T) {
		held, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "g").For(lease))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		for name, arg := range map[string]entroq.ModifyArg{
			"change": held[0].Change(entroq.WithContent("stolen")),
			"delete": held[0].Delete(),
			"insert": entroq.PuttingDocInto(ns, entroq.WithKeys("g", "c")),
		} {
			_, err := client.Modify(ctx, arg, entroq.ModifyAs(intruder))
			if depErr, ok := entroq.AsDependency(err); !ok || !depErr.HasClaimedDocs() {
				t.Errorf("Intruder %s in a held group: want a claim error, got %v", name, err)
			}
		}
		if _, err := client.Modify(ctx, held[0].Depend(), entroq.ModifyAs(intruder)); err != nil {
			t.Errorf("Intruder depend on a held group: %v", err)
		}
		if _, err := client.ClaimDocs(ctx, &entroq.DocClaim{Namespace: ns, Key: "g", Claimant: intruder, Duration: lease}); !entroq.IsDependency(err) {
			t.Errorf("Intruder claim of a held group: want a dependency error, got %v", err)
		}

		// Renewing keeps the group held; committing without renewing releases it.
		resp, err := client.Modify(ctx, held[0].Change(entroq.WithDocArrivalTimeBy(lease)))
		if err != nil {
			t.Fatalf("Holder renew: %v", err)
		}
		renewed := resp.ChangedDocs[0]
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("g", "c")), entroq.ModifyAs(intruder)); !entroq.IsDependency(err) {
			t.Errorf("Intruder insert after the holder renewed: want a claim error, got %v", err)
		}
		if _, err := client.Modify(ctx, renewed.Change(entroq.WithContent("done"))); err != nil {
			t.Fatalf("Holder release: %v", err)
		}
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("g", "c")), entroq.ModifyAs(intruder)); err != nil {
			t.Errorf("Insert after the holder released: %v", err)
		}
	})

	t.Run("an empty group can be claimed", func(t *testing.T) {
		held, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "empty").For(lease))
		if err != nil {
			t.Fatalf("Claim of an empty group: %v", err)
		}
		if len(held) != 0 {
			t.Fatalf("Claim of an empty group returned %d docs", len(held))
		}
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("empty", "x")), entroq.ModifyAs(intruder)); !entroq.IsDependency(err) {
			t.Errorf("Intruder insert into a claimed empty group: want a claim error, got %v", err)
		}
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("empty", "x"))); err != nil {
			t.Errorf("Holder insert into its claimed empty group: %v", err)
		}
	})

	t.Run("inserts into an unheld group leave its version", func(t *testing.T) {
		before := readGroup("g")
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithKeys("g", "appended")), entroq.ModifyAs(intruder)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		after := readGroup("g")
		if len(after) != len(before)+1 {
			t.Fatalf("Group has %d members after an insert, want %d", len(after), len(before)+1)
		}
		for _, d := range after {
			if d.Version != before[0].Version {
				t.Errorf("Member %q at version %d after an insert, want %d", d.ID, d.Version, before[0].Version)
			}
		}
		// Nothing read before the insert changed, so a depend on it holds.
		if _, err := client.Modify(ctx, before[0].Depend()); err != nil {
			t.Errorf("Depend on a read taken before an insert: %v", err)
		}
		// A delete can falsify what was read, so it moves the version.
		if _, err := client.Modify(ctx, before[0].Delete()); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := client.Modify(ctx, before[1].Depend()); !entroq.IsDependency(err) {
			t.Errorf("Depend on a read taken before a delete: want a dependency error, got %v", err)
		}
	})

	t.Run("concurrent inserts into one group", func(t *testing.T) {
		const writers = 8
		insertAll := func(opts func(i int) []entroq.DocOpt) []error {
			errs := make([]error, writers)
			var wg sync.WaitGroup
			for i := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[i] = client.Modify(ctx, entroq.PuttingDocInto(ns, opts(i)...))
				}()
			}
			wg.Wait()
			return errs
		}

		for i, err := range insertAll(func(i int) []entroq.DocOpt {
			return []entroq.DocOpt{entroq.WithKeys("fan", fmt.Sprint(i))}
		}) {
			if err != nil {
				t.Errorf("Insert %d of distinct docs: %v", i, err)
			}
		}
		if n := len(readGroup("fan")); n != writers {
			t.Errorf("Group has %d members after %d inserts", n, writers)
		}

		// Inserts of one ID share the group but collide on the doc: one wins,
		// and the rest learn the ID is taken.
		won := 0
		for i, err := range insertAll(func(int) []entroq.DocOpt {
			return []entroq.DocOpt{entroq.WithIDKeys("same", "fan", "same")}
		}) {
			if err == nil {
				won++
				continue
			}
			if depErr, ok := entroq.AsDependency(err); !ok || len(depErr.DocInserts) != 1 {
				t.Errorf("Insert %d of a taken ID: want an insert collision, got %v", i, err)
			}
		}
		if won != 1 {
			t.Errorf("Inserts of one ID: %d succeeded, want 1", won)
		}
	})
}

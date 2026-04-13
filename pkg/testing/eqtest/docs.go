package eqtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
	"golang.org/x/sync/errgroup"
)

// SimpleDocLifecycle tests basic insertion, change, and deletion of a doc.
func SimpleDocLifecycle(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "simple_doc")
	id := "doc-1"
	val := json.RawMessage(`{"foo":"bar"}`)

	// Insert a doc
	resp, err := client.Modify(ctx, entroq.CreatingIn(ns,
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
	if string(changed.Content) != string(newVal) {
		t.Errorf("Changed content: want %s, got %s", newVal, changed.Content)
	}
	if changed.Version != res.Version+1 {
		t.Errorf("Changed version: want %v, got %v", res.Version+1, changed.Version)
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
	_, err = client.Modify(ctx, entroq.DeletingDoc(changed))
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
		entroq.CreatingIn(ns,
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
		args = append(args, entroq.CreatingIn(ns,
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
	for i := 0; i < numDocs; i++ {
		setupArgs = append(setupArgs, entroq.CreatingIn(ns,
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
	for w := 0; w < numWorkers; w++ {
		g.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			for i := 0; i < iterations; i++ {
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
	for i := 0; i < numItems; i++ {
		id := fmt.Sprintf("item-%d", i)
		if _, err := client.Modify(ctx,
			entroq.InsertingInto(q, entroq.WithID(id), entroq.WithValue(0)),
			entroq.CreatingIn(ns,
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
	for w := 0; w < numWorkers; w++ {
		g.Go(func() error {
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			for i := 0; i < iterations; i++ {
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
		json.Unmarshal(task.Value, &v)
		tSum += v
	}
	for _, r := range docs {
		var v int
		json.Unmarshal(r.Content, &v)
		rSum += v
	}

	expected := numWorkers * iterations
	if tSum != expected || rSum != expected {
		t.Errorf("Sum mismatch: tasks=%d, docs=%d, want=%d", tSum, rSum, expected)
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
	copyA.ExpiresAt = b.ExpiresAt
	return cmp.Diff(&copyA, b)
}

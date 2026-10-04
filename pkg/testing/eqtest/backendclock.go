package eqtest

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// ArrivalResolvesOnBackendClock holds a backend to the arrival half of the
// Modify contract: a write names its arrival as a duration, and the backend
// resolves it against ITS OWN now.
//
// The assertion is At minus Modified, which needs no clock of its own and no
// tolerance. A backend stamps both from the one reading it takes for the
// modification, so the difference is exactly the duration asked for -- unless
// the arrival was anchored somewhere else, in which case it is off by whatever
// the two clocks disagree by. That is the failure this exists to catch, and it
// is invisible to any in-process backend, where the caller's clock and the
// backend's are the same one: only a backend across a boundary, eqpg in a
// container or anything behind the service, can be caught by it. So the
// contract is written clock-free and run everywhere rather than written for
// the one backend that can currently fail it.
//
// Comparing a stored At against a separate Time() round trip cannot do this
// job: two readings taken milliseconds apart need a tolerance, and the error
// being hunted is smaller than the tolerance has to be.
//
// Durations here are deliberately not round, so a backend that defaults or
// truncates an arrival cannot pass by luck.
func ArrivalResolvesOnBackendClock(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	queue := path.Join(qPrefix, "arrival_on_backend_clock")
	ns := path.Join(qPrefix, "arrival_on_backend_clock_docs")

	// held reports how long after it was written a task or doc is held for.
	// Zero means it was written available.
	const insertBy = 37 * time.Second
	const changeBy = 41 * time.Second
	const renewBy = 43 * time.Second
	const docBy = 47 * time.Second

	t.Run("an inserted task arrives the asked-for duration after it is written", func(t *testing.T) {
		resp, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(insertBy)))
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		task := resp.InsertedTasks[0]
		if got := task.At.Sub(task.Modified); got != insertBy {
			t.Errorf("At minus Modified is %v, want %v: the arrival did not resolve against the backend's own now", got, insertBy)
		}
		if task.Claimant == "" {
			t.Error("a task inserted to arrive in the future has no claimant: the writer holds what is not yet available")
		}
	})

	t.Run("an insert naming no arrival is written available", func(t *testing.T) {
		resp, err := client.Modify(ctx, entroq.InsertingInto(queue))
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		task := resp.InsertedTasks[0]
		if !task.At.Equal(task.Modified) {
			t.Errorf("At is %v and Modified is %v, want them equal: an arrival of zero means now, and now is when it was written", task.At, task.Modified)
		}
		if task.Claimant != "" {
			t.Errorf("an available task has claimant %q, want none", task.Claimant)
		}
	})

	t.Run("a changed task arrives the asked-for duration after it is written", func(t *testing.T) {
		resp, err := client.Modify(ctx, entroq.InsertingInto(queue))
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		resp, err = client.Modify(ctx, resp.InsertedTasks[0].Change(entroq.ArrivalTimeBy(changeBy)))
		if err != nil {
			t.Fatalf("Change: %v", err)
		}
		task := resp.ChangedTasks[0]
		if got := task.At.Sub(task.Modified); got != changeBy {
			t.Errorf("At minus Modified is %v, want %v", got, changeBy)
		}
	})

	t.Run("a change naming no arrival releases at the write", func(t *testing.T) {
		resp, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(insertBy)))
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		resp, err = client.Modify(ctx, resp.InsertedTasks[0].Change())
		if err != nil {
			t.Fatalf("Change: %v", err)
		}
		task := resp.ChangedTasks[0]
		if !task.At.Equal(task.Modified) {
			t.Errorf("At is %v and Modified is %v, want them equal: a change releases unless it names an arrival", task.At, task.Modified)
		}
		if task.Claimant != "" {
			t.Errorf("a released task has claimant %q, want none", task.Claimant)
		}
	})

	t.Run("a renewal holds for the asked-for duration after it is written", func(t *testing.T) {
		resp, err := client.Modify(ctx, entroq.InsertingInto(queue))
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		resp, err = client.UpdateArrival(ctx, entroq.ReadyIn(renewBy).Tasks(resp.InsertedTasks[0]))
		if err != nil {
			t.Fatalf("UpdateArrival: %v", err)
		}
		task := resp.ChangedTasks[0]
		if got := task.At.Sub(task.Modified); got != renewBy {
			t.Errorf("At minus Modified is %v, want %v: a lease change resolves on the backend's clock as any write does", got, renewBy)
		}
	})

	t.Run("an inserted doc holds its set for the asked-for duration", func(t *testing.T) {
		resp, err := client.Modify(ctx, entroq.PuttingDocInto(ns,
			entroq.WithKeys("arrival", "a"),
			entroq.WithDocArrivalTimeBy(docBy)))
		if err != nil {
			t.Fatalf("Insert doc: %v", err)
		}
		doc := resp.InsertedDocs[0]
		if got := doc.At.Sub(doc.Modified); got != docBy {
			t.Errorf("At minus Modified is %v, want %v: a doc arrival resolves on the backend's own now too", got, docBy)
		}
		if doc.Claimant == "" {
			t.Error("a doc inserted to arrive in the future has no claimant: the writer holds the set")
		}
	})
}

package eqtest

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// UpdateArrival checks the arrival update contract: tasks and doc sets the
// caller holds are renewed or released together, each moving one version and
// nothing else, and an update that names anything the caller does not hold at
// the version named changes nothing.
func UpdateArrival(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := func(key string) string { return path.Join(qPrefix, "arrival", key) }
	ns := path.Join(qPrefix, "arrival", "docs")
	const intruder = "intruder"

	// claim inserts a task in its own queue and a set of two docs, both
	// under key, and claims both.
	claim := func(t *testing.T, key string) (*entroq.Task, *entroq.DocSet) {
		t.Helper()
		if _, err := client.Modify(ctx,
			entroq.InsertingInto(queue(key), entroq.WithValue(key)),
			entroq.PuttingDocInto(ns, entroq.WithKeys(key, "a")),
			entroq.PuttingDocInto(ns, entroq.WithKeys(key, "b")),
		); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		task, err := client.Claim(ctx, entroq.From(queue(key)), entroq.ClaimFor(time.Minute))
		if err != nil {
			t.Fatalf("Claim task: %v", err)
		}
		set, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, key).For(time.Minute))
		if err != nil {
			t.Fatalf("Claim set: %v", err)
		}
		return task, set
	}

	t.Run("renew moves one version and keeps the claim", func(t *testing.T) {
		task, set := claim(t, "renew")
		before := time.Now()
		resp, err := client.UpdateArrival(ctx, entroq.ReadyIn(time.Hour).Tasks(task).Docs(set))
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if len(resp.ChangedTasks) != 1 || len(resp.ChangedSets) != 1 {
			t.Fatalf("Renew response: want one task and one set, got %+v", resp)
		}
		rt, rg := resp.ChangedTasks[0], resp.ChangedSets[0]
		if rt.Version != task.Version+1 || rt.Claimant != client.ClientID || rt.At.Before(before.Add(59*time.Minute)) {
			t.Errorf("Renewed task: want version %d held by %s for an hour, got %+v", task.Version+1, client.ClientID, rt)
		}
		if rt.Claims != task.Claims || rt.Attempt != task.Attempt || string(rt.Value) != string(task.Value) {
			t.Errorf("Renewed task changed more than its arrival: before %+v, after %+v", task, rt)
		}
		if rg.Version != set.Version+1 || rg.Claimant != client.ClientID || rg.At.Before(before.Add(59*time.Minute)) || rg.NumDocs != 2 {
			t.Errorf("Renewed set: want version %d held for an hour with 2 docs, got %+v", set.Version+1, rg)
		}
		// Every write moves the version, so what was read before is stale.
		if _, err := client.Modify(ctx, task.Depend()); !entroq.IsDependency(err) {
			t.Errorf("Depend on the task at its version before renewal: want a dependency error, got %v", err)
		}
		if _, err := client.Modify(ctx, rt.Delete(), set.Docs[0].Delete()); !entroq.IsDependency(err) {
			t.Errorf("Delete a doc at its version before renewal: want a dependency error, got %v", err)
		}
		if _, err := client.ClaimDocs(ctx, &entroq.DocClaim{Namespace: ns, Key: "renew", Claimant: intruder, Duration: time.Minute}); !entroq.IsDependency(err) {
			t.Errorf("Intruder claim of a renewed set: want a dependency error, got %v", err)
		}
	})

	t.Run("release makes items ready now", func(t *testing.T) {
		task, set := claim(t, "release")
		resp, err := client.UpdateArrival(ctx, entroq.ReadyNow().Tasks(task).Docs(set))
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
		if rt := resp.ChangedTasks[0]; rt.Version != task.Version+1 || rt.Claimant != "" || rt.At.After(time.Now()) {
			t.Errorf("Released task: want version %d, unclaimed, ready now, got %+v", task.Version+1, rt)
		}
		if rg := resp.ChangedSets[0]; rg.Version != set.Version+1 || rg.Claimant != "" {
			t.Errorf("Released set: want version %d, unheld, got %+v", set.Version+1, rg)
		}
		if got, err := client.TryClaim(ctx, entroq.From(queue("release"))); err != nil || got == nil || got.ID != task.ID {
			t.Errorf("Claim after release: want task %s, got %v, %v", task.ID, got, err)
		}
		if _, err := client.ClaimDocs(ctx, &entroq.DocClaim{Namespace: ns, Key: "release", Claimant: intruder, Duration: time.Minute}); err != nil {
			t.Errorf("Intruder claim of a released set: %v", err)
		}
	})

	t.Run("entries with different durations apply together", func(t *testing.T) {
		task, set := claim(t, "mixed")
		resp, err := client.UpdateArrival(ctx,
			entroq.ReadyIn(time.Hour).Tasks(task),
			entroq.ReadyNow().Docs(set),
		)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if resp.ChangedTasks[0].Claimant != client.ClientID || resp.ChangedSets[0].Claimant != "" {
			t.Errorf("Mixed update: want the task kept and the set released, got %+v and %+v", resp.ChangedTasks[0], resp.ChangedSets[0])
		}
	})

	t.Run("arrivals commit with other work", func(t *testing.T) {
		_, set := claim(t, "commit")
		resp, err := client.Modify(ctx,
			entroq.Arriving(entroq.ReadyIn(time.Hour).Docs(set)),
			entroq.InsertingInto(queue("commit-out"), entroq.WithValue("out")),
		)
		if err != nil {
			t.Fatalf("Modify: %v", err)
		}
		if len(resp.InsertedTasks) != 1 || len(resp.ChangedSets) != 1 || resp.ChangedSets[0].Version != set.Version+1 {
			t.Errorf("Arrival with an insert: want both applied, got %+v", resp)
		}
	})

	t.Run("an empty set renews", func(t *testing.T) {
		set, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "empty").For(time.Minute))
		if err != nil {
			t.Fatalf("Claim empty set: %v", err)
		}
		resp, err := client.UpdateArrival(ctx, entroq.ReadyIn(time.Hour).Docs(set))
		if err != nil {
			t.Fatalf("Renew empty set: %v", err)
		}
		if rg := resp.ChangedSets[0]; rg.Version != set.Version+1 || rg.NumDocs != 0 || rg.Claimant != client.ClientID {
			t.Errorf("Renewed empty set: want version %d, no docs, still held, got %+v", set.Version+1, rg)
		}
	})

	t.Run("anything not held fails the whole update", func(t *testing.T) {
		task, set := claim(t, "whole")
		stale := *set
		stale.Version--
		_, err := client.UpdateArrival(ctx, entroq.ReadyIn(time.Hour).Tasks(task).Docs(&stale))
		depErr, ok := entroq.AsDependency(err)
		if !ok || len(depErr.DocArrives) != 1 || !depErr.DocArrives[0].IsSetRef() || depErr.DocArrives[0].Version != set.Version {
			t.Fatalf("Update naming a stale set: want the set reported at its current version %d, got %v", set.Version, err)
		}
		staleTask := *task
		staleTask.Version--
		_, err = client.UpdateArrival(ctx, entroq.ReadyIn(time.Hour).Tasks(&staleTask))
		if depErr, ok := entroq.AsDependency(err); !ok || len(depErr.Arrives) != 1 || len(depErr.Changes) != 0 {
			t.Errorf("Update naming a stale task: want it reported as an arrival, got %v", err)
		}
		// Nothing changed: the task is still at the version it was claimed at.
		if _, err := client.Modify(ctx, task.Depend()); err != nil {
			t.Errorf("Task after a failed update: want it unchanged, got %v", err)
		}

		// Another claimant cannot move what this one holds.
		_, err = client.Modify(ctx, entroq.Arriving(entroq.ReadyIn(time.Hour).Tasks(task)), entroq.ModifyAs(intruder))
		if depErr, ok := entroq.AsDependency(err); !ok || len(depErr.Claims) != 1 {
			t.Errorf("Task update by someone else: want the task reported as held, got %v", err)
		}
		_, err = client.Modify(ctx, entroq.Arriving(entroq.ReadyIn(time.Hour).Docs(set)), entroq.ModifyAs(intruder))
		// The failure names the set; who holds it is not carried over every
		// transport.
		if depErr, ok := entroq.AsDependency(err); !ok || len(depErr.DocClaims) != 1 || !depErr.DocClaims[0].IsSetRef() ||
			depErr.DocClaims[0].Namespace != ns || depErr.DocClaims[0].Key != "whole" {
			t.Errorf("Set update by someone else: want the set reported as held, got %v", err)
		}
	})

	t.Run("an update must name something, once", func(t *testing.T) {
		if _, err := client.UpdateArrival(ctx, entroq.ReadyNow()); !entroq.IsInvalidArgument(err) {
			t.Errorf("Update naming nothing: want an invalid argument, got %v", err)
		}
		task, _ := claim(t, "twice")
		if _, err := client.UpdateArrival(ctx, entroq.ReadyNow().Tasks(task), entroq.ReadyIn(time.Hour).Tasks(task)); !entroq.IsInvalidArgument(err) {
			t.Errorf("Update naming a task twice: want an invalid argument, got %v", err)
		}
	})
}

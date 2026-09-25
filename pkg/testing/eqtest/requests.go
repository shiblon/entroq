package eqtest

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// InvalidRequests checks that the client rejects malformed requests as
// invalid arguments before they reach a backend, and fills in the defaults it
// promises: a zero claim duration means the default one.
func InvalidRequests(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "invalid_requests")
	ns := path.Join(qPrefix, "invalid_requests_docs")

	for name, err := range map[string]error{
		"claim from no queues": second(client.TryClaim(ctx)),
		"claim with no claimant": second(client.TryClaim(ctx,
			entroq.From(queue), entroq.WithClaimant(""))),
		"claim for a negative duration": second(client.TryClaim(ctx,
			entroq.From(queue), entroq.ClaimFor(-time.Second))),
		"tasks with no queue or IDs":   second(client.Tasks(ctx, "")),
		"docs with no namespace":       second(client.Docs(ctx, &entroq.DocQuery{KeyExact: "k"})),
		"docs by ID with no namespace": second(client.Docs(ctx, &entroq.DocQuery{IDs: []string{"a"}})),
		"doc claim with no namespace":  second(client.ClaimDocs(ctx, entroq.ClaimKey("", "k"))),
		"doc claim with no key":        second(client.ClaimDocs(ctx, entroq.ClaimKey(ns, ""))),
		"doc claim, negative duration": second(client.ClaimDocs(ctx, entroq.ClaimKey(ns, "k").For(-time.Second))),
	} {
		if !entroq.IsInvalidArgument(err) {
			t.Errorf("%s: want an invalid argument, got %v", name, err)
		}
	}

	// Task IDs are unique across queues, so a lookup by ID alone is a valid
	// request.
	if _, err := client.Tasks(ctx, "", entroq.WithTaskID("no-such-task")); err != nil {
		t.Errorf("Tasks by ID alone: %v", err)
	}

	// A zero duration is the default claim duration, not an immediately
	// expired claim.
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	before := time.Now()
	task, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(0))
	if err != nil || task == nil {
		t.Fatalf("Claim for a zero duration: %v, %v", task, err)
	}
	if !task.At.After(before.Add(entroq.DefaultClaimDuration / 2)) {
		t.Errorf("Claim for a zero duration: held until %v, want about %v from %v", task.At, entroq.DefaultClaimDuration, before)
	}
	docs, err := client.ClaimDocs(ctx, entroq.ClaimKey(ns, "zero").For(0))
	if err != nil {
		t.Fatalf("Doc claim for a zero duration: %v", err)
	}
	if _, err := client.ClaimDocs(ctx, &entroq.DocClaim{Namespace: ns, Key: "zero", Claimant: "intruder"}); !entroq.IsDependency(err) {
		t.Errorf("Doc claim for a zero duration: want the group held, another claim got %v (docs %v)", err, docs)
	}
}

// BackendRejectsInvalidRequests calls a backend directly with requests the
// client would have rejected, and checks that the backend refuses each as an
// invalid argument rather than acting on it. Through the gRPC backend, the
// service's checks refuse them.
func BackendRejectsInvalidRequests(ctx context.Context, t *testing.T, backend entroq.Backend, qPrefix string) {
	queue := path.Join(qPrefix, "backend_invalid_requests")
	ns := path.Join(qPrefix, "backend_invalid_requests_docs")

	for name, err := range map[string]error{
		"claim from no queues": second(backend.TryClaim(ctx,
			&entroq.ClaimQuery{Claimant: "me", Duration: time.Minute})),
		"claim with no claimant": second(backend.TryClaim(ctx,
			&entroq.ClaimQuery{Queues: []string{queue}, Duration: time.Minute})),
		"tasks with no queue or IDs": second(backend.Tasks(ctx, &entroq.TasksQuery{})),
		"docs with no namespace":     second(backend.Docs(ctx, &entroq.DocQuery{IDs: []string{"a"}})),
		"doc claim with no claimant": second(backend.ClaimDocs(ctx,
			&entroq.DocClaim{Namespace: ns, Key: "k", Duration: time.Minute})),
	} {
		if !entroq.IsInvalidArgument(err) {
			t.Errorf("%s: want an invalid argument, got %v", name, err)
		}
	}
}

// StorageRejectsZeroDurations checks that a storage backend refuses a claim
// with no duration. The client and the service turn a zero duration into the
// default before a claim reaches storage, so one arriving here bypassed them,
// and acting on it would claim a task or group that is available again at
// once.
func StorageRejectsZeroDurations(ctx context.Context, t *testing.T, backend entroq.Backend, qPrefix string) {
	queue := path.Join(qPrefix, "storage_zero_durations")
	ns := path.Join(qPrefix, "storage_zero_durations_docs")
	for name, err := range map[string]error{
		"claim": second(backend.TryClaim(ctx,
			&entroq.ClaimQuery{Queues: []string{queue}, Claimant: "me"})),
		"doc claim": second(backend.ClaimDocs(ctx,
			&entroq.DocClaim{Namespace: ns, Key: "k", Claimant: "me"})),
	} {
		if !entroq.IsInvalidArgument(err) {
			t.Errorf("%s for no duration: want an invalid argument, got %v", name, err)
		}
	}
}

// second returns the error of a two-result call.
func second[T any](_ T, err error) error {
	return err
}

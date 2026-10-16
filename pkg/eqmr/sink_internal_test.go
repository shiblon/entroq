package eqmr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// TestDocumentSinkFirstCommitWins pins the Sink contract on the document sink:
// once a partition is committed, a commit of a different attempt leaves it as
// it was, and so does a repeat of the same one; and Sweep removes every
// uncommitted attempt while never touching the committed one.
func TestDocumentSinkFirstCommitWins(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	const run = "/sinktest"
	sink := newDocumentSink(eq, run)
	stage := func(kvs ...*KV) []byte {
		t.Helper()
		w, err := sink.Stage(ctx, run, 0, 1)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		for _, kv := range kvs {
			if err := w.Write(ctx, kv.Key, kv.Value); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		staged, err := w.Close(ctx)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		return staged
	}
	first := stage(NewKV("a", "first"))
	second := stage(NewKV("a", "second"))

	for i, staged := range [][]byte{first, second, first} {
		if err := sink.Commit(ctx, run, 0, staged); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	got, err := sink.readPartition(ctx, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].Value != "first" {
		t.Fatalf("partition 0 = %v, want the first commit", got)
	}

	for i := range 2 {
		if err := sink.Sweep(ctx, run); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	got, err = sink.readPartition(ctx, 0)
	if err != nil {
		t.Fatalf("read after sweep: %v", err)
	}
	if len(got) != 1 || got[0].Value != "first" {
		t.Fatalf("partition 0 after sweep = %v, want the first commit intact", got)
	}
	staged, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: run, KeyStart: documentSinkStaging, KeyEnd: prefixEnd(documentSinkStaging)})
	if err != nil {
		t.Fatalf("list staged: %v", err)
	}
	if len(staged) != 1 {
		t.Errorf("%d staged chunks remain after sweep, want only the committed one", len(staged))
	}
	for range 2 {
		if err := sink.Finish(ctx, run, 1, nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}
	if _, err := sink.manifest(ctx); err != nil {
		t.Fatalf("manifest after finish: %v", err)
	}
}

// TestRetryIfUnavailable pins the error classification controller and worker
// reads share: only a backend known to be temporarily unavailable is retried.
func TestRetryIfUnavailable(t *testing.T) {
	wrapped := fmt.Errorf("read split: %w", entroq.Unavailablef("server restarting"))
	if _, ok := worker.AsRetry(retryIfUnavailable(wrapped)); !ok {
		t.Errorf("unavailable backend was not retried")
	}
	plain := errors.New("malformed query")
	if got := retryIfUnavailable(plain); got != plain {
		t.Errorf("unexplained error became %v, want it returned unchanged", got)
	}
	if retryIfUnavailable(nil) != nil {
		t.Error("nil error was not nil")
	}
}

// TestDocumentSinkRefusesOtherRuns pins that a document sink, which belongs to
// one run's namespace, refuses every operation for another run as permanent
// rather than writing into the wrong place.
func TestDocumentSinkRefusesOtherRuns(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()
	sink := newDocumentSink(eq, "/mine")
	_, stageErr := sink.Stage(ctx, "/theirs", 0, 1)
	for what, err := range map[string]error{
		"stage":  stageErr,
		"commit": sink.Commit(ctx, "/theirs", 0, []byte(`{}`)),
		"finish": sink.Finish(ctx, "/theirs", 1, nil),
		"sweep":  sink.Sweep(ctx, "/theirs"),
	} {
		if _, ok := worker.AsMove(err); !ok {
			t.Errorf("%s for another run: got %v, want a permanent refusal", what, err)
		}
	}
}

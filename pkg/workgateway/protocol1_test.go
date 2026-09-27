package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/version"
)

// These tests cover what protocol 1 added: the hello, doc sets in doWork,
// the "error" outcome, and the error-queue and retry-delay registration.

func TestBridge_HelloFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := newSession(t, ctx, newEQ(t, ctx), workCfg(), time.Second)
	if s.hello.Protocol != Protocol || s.hello.Version != version.Version {
		t.Errorf("hello: got protocol %d version %q, want %d %q", s.hello.Protocol, s.hello.Version, Protocol, version.Version)
	}
	s.stop()
}

// TestBridge_HelloBeforeRegistrationError checks that even a registration the
// gateway refuses gets the hello first, so a client can tell a registration
// it got wrong from a gateway it cannot talk to.
func TestBridge_HelloBeforeRegistrationError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := newSession(t, ctx, newEQ(t, ctx), Config{Work: true}, time.Second) // no queues
	if ee, ok := AsExit(s.wait()); !ok || ee.Class != ExitCaller {
		t.Fatalf("want a caller exit for a registration with no queues, got %v", ee)
	}
}

// TestBridge_DoWorkSets checks that doWork carries each claimed set, in
// claim order, with its docs and its version and claim, including a set
// claimed with no docs, and still carries the flat docs as before.
func TestBridge_DoWorkSets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("in", entroq.WithValue("hello")),
		entroq.PuttingDocInto("ns", entroq.WithKeys("full", "a")),
		entroq.PuttingDocInto("ns", entroq.WithKeys("full", "b")),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	cfg := workCfg()
	cfg.TakeDocs = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var td takeDocsMsg
	s.c.recv(&td)
	s.c.send(docsMsg{Type: msgDocs, Claims: []docClaim{{Namespace: "ns", Key: "full"}, {Namespace: "ns", Key: "empty"}}})

	var dw doWorkMsg
	s.c.recv(&dw)
	if len(dw.Sets) != 2 {
		t.Fatalf("doWork carried %d sets, want 2", len(dw.Sets))
	}
	// Claims are taken sorted by namespace and key: "empty" before "full".
	empty, full := dw.Sets[0], dw.Sets[1]
	if empty.Key != "empty" || len(empty.Docs) != 0 || empty.Claimant == "" {
		t.Errorf("empty set: got key %q, %d docs, claimant %q", empty.Key, len(empty.Docs), empty.Claimant)
	}
	if full.Key != "full" || len(full.Docs) != 2 {
		t.Fatalf("full set: got key %q, %d docs", full.Key, len(full.Docs))
	}
	for _, d := range full.Docs {
		if d.Version != full.Version || d.Claimant != full.Claimant {
			t.Errorf("member %q: version %d claimant %q, set has %d %q", d.Id, d.Version, d.Claimant, full.Version, full.Claimant)
		}
	}
	if len(dw.Docs) != 2 {
		t.Errorf("flat docs: got %d, want 2", len(dw.Docs))
	}
	s.c.send(okResult(deleteTask(dw.Task.Task)))
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.stop()
}

// TestBridge_HandlerError checks the "error" outcome: the gateway reports the
// handler's failure over the error channel, stops with the caller class, and
// leaves the task claimed for its lease to release, as the Go worker does for
// a handler error it does not understand.
func TestBridge_HandlerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeError, Message: "TypeError: x is undefined"}})

	var em errorMsg
	s.c.recv(&em)
	if em.Class != ExitCaller.String() {
		t.Errorf("error class = %q, want %q", em.Class, ExitCaller.String())
	}
	err := s.wait()
	var he *HandlerError
	if ee, ok := AsExit(err); !ok || ee.Class != ExitCaller || !errors.As(err, &he) || he.Message != "TypeError: x is undefined" {
		t.Fatalf("want a caller exit carrying the handler error, got %v", err)
	}
	tasks, err := eq.Tasks(ctx, "in")
	if err != nil || len(tasks) != 1 || !tasks[0].At.After(time.Now()) {
		t.Errorf("task after a handler error: want it still held in its queue, got %v, %v", tasks, err)
	}
}

// TestBridge_ErrorQueueTemplate checks that a move with no destination goes to
// the registered error queue, with "{inbox}" standing for the task's queue.
func TestBridge_ErrorQueueTemplate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	cfg := workCfg()
	cfg.ErrorQueue = "{inbox}/quarantine"
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeMove, Message: "bad input"}})
	waitForTask(t, ctx, eq, "in/quarantine")
	s.stop()
}

// TestBridge_RetryDelay checks that a retry with no delay of its own waits the
// registered base retry delay.
func TestBridge_RetryDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	cfg := workCfg()
	cfg.RetryDelay = time.Hour
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeRetry, Message: "later"}})
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := eq.Tasks(ctx, "in")
		if err != nil {
			t.Fatalf("tasks: %v", err)
		}
		if len(tasks) == 1 && tasks[0].Attempt == 1 {
			// Base delays are randomized, but never below half.
			if at := tasks[0].At; at.Before(time.Now().Add(20 * time.Minute)) {
				t.Errorf("retried task available at %v, want about an hour out", at)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("task never retried")
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.stop()
}

// TestWireGroupRoundTrip checks the set encoding: the lock's protojson fields
// with the docs beside them, and an empty set's docs as [] rather than null.
func TestWireGroupRoundTrip(t *testing.T) {
	g := wireSet{Doc: &pb.Doc{Namespace: "ns", Key: "k", Version: 3, Claimant: "me", AtMs: 42}}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["docs"]) != "[]" || string(fields["atMs"]) != `"42"` || string(fields["key"]) != `"k"` {
		t.Errorf("encoded set: %s", b)
	}
	g.Docs = []wireDoc{{&pb.Doc{Namespace: "ns", Id: "a", Key: "k", Version: 3}}}
	if b, err = json.Marshal(g); err != nil {
		t.Fatal(err)
	}
	var back wireSet
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Key != "k" || back.Version != 3 || back.AtMs != 42 || len(back.Docs) != 1 || back.Docs[0].Id != "a" {
		t.Errorf("round trip: got %+v, docs %v", back.Doc, back.Docs)
	}
}

func waitForTask(t *testing.T, ctx context.Context, eq *entroq.EntroQ, q string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := eq.Tasks(ctx, q)
		if err != nil {
			t.Fatalf("tasks %q: %v", q, err)
		}
		if len(tasks) > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no task ever reached %q", q)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

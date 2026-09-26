package workgateway

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// These tests cover the failure contract: what the gateway does when the
// worker hangs up, speaks out of turn, or loses its claim mid-task. Each is one
// row of the table in docs/workgateway-protocol.md.

// TestContract_HangUpWhileIdle: a worker that hangs up while the gateway waits
// for a task stops the session at once, rather than when a task arrives for a
// worker that is gone.
func TestContract_HangUpWhileIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	s := newSession(t, ctx, eq, workCfg(), time.Minute)

	time.Sleep(50 * time.Millisecond) // let the gateway block in its claim
	s.closeClient()
	if err := s.wait(); err != nil {
		t.Fatalf("a hang-up while idle should be a clean stop, got: %v", err)
	}
	insertTask(t, ctx, eq, "in", "late")
	tasks, err := eq.Tasks(ctx, "in")
	if err != nil || len(tasks) != 1 || tasks[0].Claims != 0 {
		t.Fatalf("a task arriving after the hang-up must stay unclaimed, got %v, %v", tasks, err)
	}
}

// TestContract_ResultThenHangUp: a worker may send its result and exit. The
// hang-up does not interrupt the commit of a result already received.
func TestContract_ResultThenHangUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	s := newSession(t, ctx, eq, workCfg(), time.Minute)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(okResult(deleteTask(dw.Task.Task)))
	s.closeClient()

	if err := s.wait(); err != nil {
		t.Fatalf("a hang-up after the result should be a clean stop, got: %v", err)
	}
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("the result sent before the hang-up must commit: %v", err)
	}
}

// TestContract_Unsolicited: a message sent with no request outstanding is a
// protocol violation, a caller fault.
func TestContract_Unsolicited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	s := newSession(t, ctx, eq, workCfg(), time.Minute)

	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeOK}})

	var em errorMsg
	s.c.recv(&em)
	if em.Type != msgError || em.Class != ExitCaller.String() {
		t.Fatalf("got %+v, want an error message of class %q", em, ExitCaller.String())
	}
	if ee, ok := AsExit(s.wait()); !ok || ee.Class != ExitCaller {
		t.Fatalf("want a caller exit, got %+v (ok=%v)", ee, ok)
	}
}

// stealClaim takes the gateway's claim on the task in q, as if its lease had
// lapsed and another worker had claimed it: the gateway's next renewal fails.
func stealClaim(t *testing.T, ctx context.Context, eq *entroq.EntroQ, q string) *entroq.Task {
	t.Helper()
	tasks, err := eq.Tasks(ctx, q)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks %q: %v, %v", q, tasks, err)
	}
	resp, err := eq.Modify(ctx, entroq.ModifyAs(tasks[0].Claimant), tasks[0].Change(entroq.ArrivalTimeBy(time.Hour)))
	if err != nil {
		t.Fatalf("steal claim: %v", err)
	}
	return resp.ChangedTasks[0]
}

// readAbort reads the abort for task dw, skipping nothing: it must be the next
// message.
func readAbort(t *testing.T, s *session, dw doWorkMsg) {
	t.Helper()
	var ab abortMsg
	s.c.recv(&ab)
	if ab.Type != msgAbort || ab.ID != dw.Task.Id || ab.Version != dw.Task.Version {
		t.Fatalf("got %+v, want an abort of task %s v%d", ab, dw.Task.Id, dw.Task.Version)
	}
}

// TestContract_AbortOnLostClaim: when the claim is lost mid-task, the worker
// is told to abort, and the result it sends anyway is not committed.
func TestContract_AbortOnLostClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	lease := 200 * time.Millisecond
	s := newSession(t, ctx, eq, workCfg(), lease)

	var dw doWorkMsg
	s.c.recv(&dw)
	stolen := stealClaim(t, ctx, eq, "in")
	readAbort(t, s, dw)
	s.c.send(okResult(deleteTask(dw.Task.Task)))

	// Until the worker keeps running after a lost claim (Tier 2, W5), the
	// lost claim ends the session with a gateway-class error.
	var em errorMsg
	s.c.recv(&em)
	s.wait()

	tasks, err := eq.Tasks(ctx, "in")
	if err != nil || len(tasks) != 1 || tasks[0].Version != stolen.Version {
		t.Fatalf("the late result must not commit: got %v, %v", tasks, err)
	}
}

// TestContract_AbortUnanswered: a worker that does not answer within a lease
// of the abort is a caller fault, and the session ends.
func TestContract_AbortUnanswered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	lease := 200 * time.Millisecond
	s := newSession(t, ctx, eq, workCfg(), lease)

	var dw doWorkMsg
	s.c.recv(&dw)
	stealClaim(t, ctx, eq, "in")
	readAbort(t, s, dw)

	var em errorMsg
	s.c.recv(&em)
	if em.Type != msgError || em.Class != ExitCaller.String() {
		t.Fatalf("got %+v, want an error message of class %q", em, ExitCaller.String())
	}
	if ee, ok := AsExit(s.wait()); !ok || ee.Class != ExitCaller {
		t.Fatalf("want a caller exit, got %+v (ok=%v)", ee, ok)
	}
}

// wantReleased checks that the one task in q is available now, not held for
// its lease, and was claimed once.
func wantReleased(t *testing.T, ctx context.Context, eq *entroq.EntroQ, q string) {
	t.Helper()
	tasks, err := eq.Tasks(ctx, q)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks %q: %v, %v", q, tasks, err)
	}
	if tasks[0].At.After(time.Now()) || tasks[0].Claims != 1 {
		t.Fatalf("want the task released after one claim, got at %v (now %v), claims %d", tasks[0].At, time.Now(), tasks[0].Claims)
	}
}

// TestContract_HangUpReleasesTask: a worker that hangs up without answering
// doWork has its task released at once, rather than left for its lease.
func TestContract_HangUpReleasesTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	s := newSession(t, ctx, eq, workCfg(), time.Minute)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.closeClient()
	if err := s.wait(); err != nil {
		t.Fatalf("a hang-up should be a clean stop, got: %v", err)
	}
	wantReleased(t, ctx, eq, "in")
}

// TestContract_HangUpInTakeDocsReleasesTask: the same, before doc acquisition.
func TestContract_HangUpInTakeDocsReleasesTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")
	cfg := workCfg()
	cfg.TakeDocs = true
	s := newSession(t, ctx, eq, cfg, time.Minute)

	var td takeDocsMsg
	s.c.recv(&td)
	s.closeClient()
	if err := s.wait(); err != nil {
		t.Fatalf("a hang-up should be a clean stop, got: %v", err)
	}
	wantReleased(t, ctx, eq, "in")
}

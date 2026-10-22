package workgateway

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// A quit is a flag any message may carry, and it always drains: the task in
// hand finishes through every turn it has left, then the session stops and
// answers its last request with a quit. These tests put the flag on each kind
// of message and check both halves -- the task in hand landed, and the task
// behind it was never claimed.

// drainFixture runs one session over pipes against an EntroQ holding two
// tasks, so a session that fails to stop has something to claim.
func drainFixture(ctx context.Context, t *testing.T) (*entroq.EntroQ, *client, chan error) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	for i := 0; i < 2; i++ {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	r, w, c := osPipes(t)
	return eq, c, served(ctx, eq, r, w)
}

// open sends conf and the ready, and returns the first message after them.
func open(t *testing.T, c *client, conf *Config) (string, *Response) {
	t.Helper()
	conf.Queues = []string{"inbox"}
	conf.ProtocolVersions = []int32{1}
	c.send(&Request{Type: ReqConfig, Config: conf})
	check := c.recv()
	if check.Type != RespReady {
		t.Fatalf("got %q, want %q", check.Type, RespReady)
	}
	c.send(&Request{Type: ReqReady, Session: check.Session})
	return check.Session, c.recv()
}

// expectType fails unless msg is of type want.
func expectType(t *testing.T, msg *Response, want ResponseType) {
	t.Helper()
	if msg.Type != want {
		t.Fatalf("got %q (%s), want %q", msg.Type, msg.Message, want)
	}
}

// stopped checks that the session ended cleanly and claimed nothing but the
// task it was handed (worked, or "" if none), and returns that task as it now
// stands, or nil if it is gone.
func stopped(ctx context.Context, t *testing.T, eq *entroq.EntroQ, done chan error, worked string) *entroq.Task {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want a clean stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not stop after a quit")
	}
	tasks, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	var found *entroq.Task
	for _, task := range tasks {
		if task.ID == worked {
			found = task
			continue
		}
		if task.Claims != 0 {
			t.Errorf("task %s was claimed %d times after the client asked to stop", task.ID, task.Claims)
		}
	}
	return found
}

func deleting(task *wireTask) *wireModReq {
	return &wireModReq{&pb.ModifyRequest{
		Deletes: []*pb.TaskID{{Queue: task.GetQueue(), Id: task.GetId(), Version: task.GetVersion()}},
	}}
}

func TestQuitOnConfigStartsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	c.send(&Request{Type: ReqConfig, Quit: true, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	expectType(t, c.recv(), RespQuit)
	stopped(ctx, t, eq, done, "")
}

func TestQuitOnReadyClaimsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	check := c.recv()
	c.send(&Request{Type: ReqReady, Session: check.Session, Quit: true})
	expectType(t, c.recv(), RespQuit)
	stopped(ctx, t, eq, done, "")
}

func TestQuitOnTakeDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, take := open(t, c, &Config{WantDocs: true})
	expectType(t, take, RespTake)
	c.send(&Request{Type: ReqTake, Session: session, Quit: true})

	// The task still comes, because quitting is finishing.
	work := c.recv()
	expectType(t, work, RespWork)
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(work.Task)})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left != nil {
		t.Error("the task in hand did not commit")
	}
}

func TestQuitOnAReadDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	c.send(&Request{Type: ReqTime, Session: session, Quit: true})
	expectType(t, c.recv(), RespTime)
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(work.Task)})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left != nil {
		t.Error("the task in hand did not commit")
	}
}

func TestQuitWithARetryLandsTheRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	c.send(&Request{
		Type: ReqModify, Session: session, Quit: true,
		Outcome: OutcomeRetry, Error: &TaskError{Message: "later"},
	})
	expectType(t, c.recv(), RespQuit)
	left := stopped(ctx, t, eq, done, work.Task.GetId())
	if left == nil || left.Attempt != 1 {
		t.Errorf("task after a retry with quit = %+v, want it back with attempt 1", left)
	}
}

func TestQuitOnDoneAfterSuccessDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, work := open(t, c, &Config{WantSuccess: true})
	expectType(t, work, RespWork)
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(work.Task)})
	expectType(t, c.recv(), RespSuccess)
	c.send(&Request{Type: ReqDone, Session: session, Quit: true})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left != nil {
		t.Error("the task in hand did not commit")
	}
}

func TestQuitOnDoneAfterDependencyLandsTheRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, work := open(t, c, &Config{WantDependency: true})
	expectType(t, work, RespWork)
	mod := deleting(work.Task)
	mod.Depends = []*pb.TaskID{{Queue: "inbox", Id: "nonexistent"}}
	c.send(&Request{Type: ReqModify, Session: session, Modification: mod})
	expectType(t, c.recv(), RespDependency)
	c.send(&Request{
		Type: ReqDone, Session: session, Quit: true,
		Outcome: OutcomeRetry, Error: &TaskError{Message: "try again"},
	})
	expectType(t, c.recv(), RespQuit)
	left := stopped(ctx, t, eq, done, work.Task.GetId())
	if left == nil || left.Attempt != 1 {
		t.Errorf("task after a dependency retry with quit = %+v, want it back with attempt 1", left)
	}
}

// TestQuitOnDoneAfterDependencyWithNoOutcomeStops: with no disposition the
// task is left to its lease, and the quit still stops the session there.
func TestQuitOnDoneAfterDependencyWithNoOutcomeStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, work := open(t, c, &Config{WantDependency: true})
	expectType(t, work, RespWork)
	mod := deleting(work.Task)
	mod.Depends = []*pb.TaskID{{Queue: "inbox", Id: "nonexistent"}}
	c.send(&Request{Type: ReqModify, Session: session, Modification: mod})
	expectType(t, c.recv(), RespDependency)
	c.send(&Request{Type: ReqDone, Session: session, Quit: true})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left == nil {
		t.Error("the task left to its lease is gone")
	}
}

// TestFatalOutcomeIsAnswered: a session that stops on the client's own verdict
// still answers the request that carried it, with the class a client branches
// on.
func TestFatalOutcomeIsAnswered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, c, done := drainFixture(ctx, t)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	c.send(&Request{Type: ReqModify, Session: session, Outcome: OutcomeFatal, Error: &TaskError{Message: "broken"}})
	last := c.recv()
	expectType(t, last, RespError)
	if last.Class != ExitCaller.String() {
		t.Errorf("class = %q, want %q", last.Class, ExitCaller.String())
	}
	select {
	case err := <-done:
		if ee, ok := AsExit(err); !ok || ee.Class != ExitCaller {
			t.Errorf("Serve = %v, want a caller fault", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not stop on a fatal")
	}
}

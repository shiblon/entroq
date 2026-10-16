package workgateway

import (
	"context"
	"testing"
	"time"
)

// A take reply may settle its task instead of naming docs, as a native take
// phase may: the disposition acts on the task, nothing is claimed, and work
// never comes.

func TestTakeRetrySettlesTheTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, take := open(t, c, &Config{WantDocs: true})
	expectType(t, take, RespTake)
	c.send(&Request{
		Type: ReqTake, Session: session,
		Outcome: OutcomeRetry, Error: &TaskError{Message: "not yet"},
	})

	// The session goes on to the other task rather than working this one.
	next := c.recv()
	expectType(t, next, RespTake)
	if next.Task.GetId() == take.Task.GetId() {
		t.Fatalf("after a retry in take, got task %s again", next.Task.GetId())
	}
	c.send(&Request{Type: ReqTake, Session: session, Quit: true, Outcome: OutcomeRetry})
	expectType(t, c.recv(), RespQuit)

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
	if len(tasks) != 2 {
		t.Fatalf("inbox holds %d tasks, want both back", len(tasks))
	}
	for _, task := range tasks {
		if task.Attempt != 1 {
			t.Errorf("task %s attempt = %d, want 1 after a retry in take", task.ID, task.Attempt)
		}
	}
}

func TestTakeMoveSettlesTheTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := drainFixture(ctx, t)

	session, take := open(t, c, &Config{WantDocs: true})
	expectType(t, take, RespTake)
	c.send(&Request{
		Type: ReqTake, Session: session, Quit: true,
		Outcome: OutcomeMove, Error: &TaskError{Message: "poison", Queue: "dead"},
	})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, take.Task.GetId()); left != nil {
		t.Errorf("task %s is still in the inbox after a move in take", left.ID)
	}
	dead, err := eq.Tasks(ctx, "dead")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != take.Task.GetId() {
		t.Errorf("dead holds %v, want task %s", dead, take.Task.GetId())
	}
}

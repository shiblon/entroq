package workgateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// A client that breaks the protocol is the caller's fault, whatever shape the
// break takes: the session ends with class caller, and the task in hand keeps
// its claim until the lease lapses, as for a fatal outcome, rather than being
// quarantined as though the handler had failed.

// faultServed runs one session over pipes against an EntroQ holding one task,
// and opens it to that task's work turn.
func faultServed(ctx context.Context, t *testing.T, options ...Option) (*entroq.EntroQ, *client, chan error, string, *Response) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	r, w, c := osPipes(t)
	// The options go to the conn as well as the server, since the conn is what
	// frames the stream.
	done := make(chan error, 1)
	go func() {
		done <- NewServer(ctx, eq, options...).ServeConn(ctx, NewStreamConn(Joining(r, w), options...))
	}()
	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	return eq, c, done, session, work
}

// expectCallerFault checks that the client was told of a caller fault, that
// the session ended as one, and that the task kept its claim.
func expectCallerFault(ctx context.Context, t *testing.T, eq *entroq.EntroQ, c *client, done chan error, work *Response) {
	t.Helper()
	last := c.recv()
	expectType(t, last, RespError)
	if last.Class != ExitCaller.String() {
		t.Errorf("answer class = %q (%s), want %q", last.Class, last.Message, ExitCaller)
	}
	select {
	case err := <-done:
		if ee, ok := AsExit(err); !ok || ee.Class != ExitCaller {
			t.Errorf("ServeConn = %v, want a caller fault", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	tasks, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != work.Task.GetId() || !tasks[0].At.After(time.Now()) {
		t.Errorf("inbox = %v, want the task in hand, still under its lease", tasks)
	}
}

func TestWrongAnswerIsTheCallersFault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, session, work := faultServed(ctx, t)

	c.send(&Request{Type: ReqTake, Session: session}) // the work turn wants a modify
	expectCallerFault(ctx, t, eq, c, done, work)
}

func TestOversizeFrameIsTheCallersFault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const max = 4 << 10
	eq, c, done, session, work := faultServed(ctx, t, WithMaxFrameBytes(max))

	c.send(&Request{Type: ReqModify, Session: session, Error: &TaskError{Message: strings.Repeat("x", 2*max)}})
	expectCallerFault(ctx, t, eq, c, done, work)
}

func TestUndecodableFrameIsTheCallersFault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, _, work := faultServed(ctx, t)

	c.send("a JSON string is not a request")
	expectCallerFault(ctx, t, eq, c, done, work)
}

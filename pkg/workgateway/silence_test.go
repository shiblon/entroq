package workgateway

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// The silence timeout is opt-in. A client that says it is working is held to
// it until it answers the task; one that never does is bounded by the work
// timeout alone. With a 100ms lease the window is 200ms, and every wait here
// outlasts it several times over.
const (
	silenceLease = 100 * time.Millisecond
	outlast      = 600 * time.Millisecond
)

func silenceFixture(ctx context.Context, t *testing.T, tasks int) (*entroq.EntroQ, *client, chan error) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	for i := 0; i < tasks; i++ {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	r, w, c := osPipes(t)
	return eq, c, served(ctx, eq, r, w, WithLease(silenceLease), WithMaxWorkTimeout(time.Minute))
}

// beat sends a working request and checks its answer.
func beat(t *testing.T, c *client, session string) {
	t.Helper()
	c.send(&Request{Type: ReqWorking, Session: session})
	expectType(t, c.recv(), RespOK)
}

// finishes answers work with a delete and a quit, and checks that it landed.
func finishes(ctx context.Context, t *testing.T, eq *entroq.EntroQ, c *client, done chan error, session string, work *Response) {
	t.Helper()
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(work.Task), Quit: true})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left != nil {
		t.Error("the answer did not commit")
	}
}

func TestCheckAnnouncesTheSilenceTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	r, w, c := osPipes(t)
	served(ctx, eq, r, w)
	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	check := c.recv()
	if want := int32(2 * entroq.DefaultClaimDuration / time.Second); check.SilenceTimeoutS != want {
		t.Errorf("silence_timeout_s = %d, want %d (two leases)", check.SilenceTimeoutS, want)
	}
}

func TestSilenceIsNotEnforcedUnlessArmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := silenceFixture(ctx, t, 1)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	// A read is not a beat, so it does not arm the window either.
	c.send(&Request{Type: ReqTime, Session: session})
	expectType(t, c.recv(), RespTime)
	time.Sleep(outlast)
	finishes(ctx, t, eq, c, done, session, work)
}

func TestSilenceAfterWorkingEndsTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := silenceFixture(ctx, t, 1)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	beat(t, c, session)
	time.Sleep(outlast)
	left := stopped(ctx, t, eq, done, work.Task.GetId())
	if left == nil || left.Attempt != 1 {
		t.Errorf("task after its client fell silent = %+v, want it back with attempt 1", left)
	}
}

func TestAnyRequestKeepsAnArmedTaskAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := silenceFixture(ctx, t, 1)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	beat(t, c, session)
	for deadline := time.Now().Add(outlast); time.Now().Before(deadline); {
		time.Sleep(silenceLease / 2)
		// Reads count once the window is armed; extra beats are harmless.
		c.send(&Request{Type: ReqTime, Session: session})
		expectType(t, c.recv(), RespTime)
		beat(t, c, session)
	}
	finishes(ctx, t, eq, c, done, session, work)
}

func TestNextTaskStartsUnarmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := silenceFixture(ctx, t, 2)

	session, first := open(t, c, &Config{})
	expectType(t, first, RespWork)
	beat(t, c, session)
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(first.Task)})
	second := c.recv()
	expectType(t, second, RespWork)
	time.Sleep(outlast)
	finishes(ctx, t, eq, c, done, session, second)
}

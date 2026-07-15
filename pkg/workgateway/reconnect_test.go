package workgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

// These tests drive the gateway against a real EntroQ gRPC client whose backend
// can be made unreachable and reachable again, exercising the supervision loop's
// ride-out end to end: a genuine codes.Unavailable travels through eqgrpc's error
// boundary, becomes entroq.IsUnavailable, and the gateway classifies it as
// transient. They are the higher-fidelity complement to the classify unit test
// and the eqgrpc unpack test.

// gate wraps a niladic gRPC dialer with an on/off switch that simulates the
// EntroQ backend going away and coming back: while closed it refuses new
// connections and kills live ones, so in-flight and subsequent RPCs fail
// Unavailable; while open it dials the in-process server. It mirrors the
// production model -- the client dials a stable name and the dialer resolves it
// to wherever the backend currently is, or nowhere while it is down.
type gate struct {
	mu    sync.Mutex
	open  bool
	dial  eqtest.Dialer
	conns []net.Conn
}

func (g *gate) set(open bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = open
	if !open {
		for _, c := range g.conns {
			c.Close()
		}
		g.conns = nil
	}
}

// dialer is the niladic dialer handed to eqgrpc.
func (g *gate) dialer() (net.Conn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return nil, fmt.Errorf("gate closed: backend unavailable")
	}
	c, err := g.dial()
	if err != nil {
		return nil, err
	}
	g.conns = append(g.conns, c)
	return c, nil
}

// gatedEQ starts an in-process eqmem-backed EntroQ gRPC service reached through a
// gate, returning the client and the gate (both torn down via t.Cleanup). The
// gate starts open.
func gatedEQ(t *testing.T, ctx context.Context) (*entroq.EntroQ, *gate) {
	t.Helper()
	stop, dial, err := eqtest.StartService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(stop)

	g := &gate{open: true, dial: dial}
	eq, err := entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(g.dialer),
		eqgrpc.WithInsecure(),
	))
	if err != nil {
		t.Fatalf("new gated client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq, g
}

// TestBridge_RidesOutAndRecovers is the headline resilience property end to end:
// while the gateway is waiting on the backend, the backend vanishes, the gateway
// reports a transient error and keeps retrying (the client sees a pause, not a
// disconnect), and once the backend returns the gateway reconnects and delivers
// the next task.
func TestBridge_RidesOutAndRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, g := gatedEQ(t, ctx)

	// The queue is empty, so the gateway blocks claiming; the default EntroQ
	// timeout is generous, so the outage is ridden out rather than escalated.
	s := newSession(t, ctx, eq, workCfg(), time.Second)

	// The backend vanishes: the blocking claim fails and the gateway reports a
	// transient error over the (still-live) client connection while it retries.
	g.set(false)
	var em errorMsg
	s.c.recv(&em)
	if em.Type != msgError || em.Class != ExitTransient.String() {
		t.Fatalf("got %+v, want a transient error message", em)
	}

	// The backend returns and a task appears. Seeding shares the same channel, so
	// retry until gRPC's post-failure backoff elapses and the reconnect lands.
	g.set(true)
	seedDeadline := time.After(10 * time.Second)
	for {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("in", entroq.WithValue("hello"))); err == nil {
			break
		}
		select {
		case <-seedDeadline:
			t.Fatal("could not seed after reopening the gate")
		case <-time.After(50 * time.Millisecond):
		}
	}
	for {
		typ, raw := s.c.recvAny()
		switch typ {
		case msgError:
			continue // still catching up from the outage
		case msgDoWork:
			var dw doWorkMsg
			if err := json.Unmarshal(raw, &dw); err != nil {
				t.Fatalf("decode doWork: %v", err)
			}
			if got := dw.Task.Value.GetStringValue(); got != "hello" {
				t.Errorf("recovered task value = %q, want %q", got, "hello")
			}
			s.c.send(okResult(deleteTask(dw.Task.Task)))
			if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
				t.Fatalf("wait queue empty after recovery: %v", err)
			}
			s.stop()
			return
		default:
			t.Fatalf("unexpected message %q during recovery", typ)
		}
	}
}

// TestBridge_EntroQTimeoutExits proves the fatal timeout: a backend that never
// returns is ridden out only for WithEntroQTimeout, after which the gateway gives
// up and exits Transient, handing the longer-horizon retry to the client's
// supervisor.
func TestBridge_EntroQTimeoutExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, g := gatedEQ(t, ctx)

	// A short ride-out window so a persistent outage escalates quickly.
	s := newSession(t, ctx, eq, workCfg(), time.Second, WithEntroQTimeout(300*time.Millisecond))

	// Drain the gateway's per-iteration error reports in the background so its
	// notify never blocks on the synchronous pipe; we only care that Run exits.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		var raw json.RawMessage
		for s.c.dec.Decode(&raw) == nil {
		}
	}()

	g.set(false) // the backend goes away and never returns

	err := s.wait()
	ee, ok := AsExit(err)
	if !ok || ee.Class != ExitTransient {
		t.Fatalf("expected a transient exit after the ride-out timeout, got %+v (ok=%v)", ee, ok)
	}
	s.closeClient() // unblock and stop the drainer
	<-drained
}

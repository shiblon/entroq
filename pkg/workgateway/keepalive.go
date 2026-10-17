package workgateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shiblon/entroq/pkg/worker"
)

// errIdleSilent is a client that did not answer an idle ready within the
// silence window. It has gone, as a hang-up has, and the session ends cleanly.
var errIdleSilent = errors.New("client did not answer an idle ready")

// keptConn is a session's conn while the worker claims. A request the gateway
// owes an answer has its answer only when a task arrives, which may be never;
// so once one has waited every, keptConn answers it with a ready, and the
// client answers that with a ready of its own, which is owed in turn. Over HTTP
// that keeps a waiting request from outliving whatever stands between the
// client and the gateway; over a pipe it tells the client the gateway is still
// there.
//
// The client has within to answer. A task that arrives meanwhile waits for that
// answer, and then answers it.
type keptConn struct {
	Conn

	ctx           context.Context
	every, within time.Duration
	ready         func() *Response

	// quit is called when the client's answer to an idle ready carries a quit,
	// and fail when it does not answer as it should. Each runs at most once.
	quit, fail func()

	mu sync.Mutex
	// owed is whether a request is waiting for its answer, and timer what
	// answers it with a ready once it has waited every.
	owed  bool
	timer *time.Timer
	// turn is closed when the idle turn in progress ends, and nil when there
	// is none.
	turn chan struct{}
	err  error
}

// Recv receives a request, which is owed an answer from then on.
func (k *keptConn) Recv(ctx context.Context) (*Request, error) {
	msg, err := k.Conn.Recv(ctx)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.owe()
	return msg, nil
}

// owe marks a request owed and starts its timer. k.mu must be held.
func (k *keptConn) owe() {
	k.owed = true
	k.timer = time.AfterFunc(k.every, k.idle)
}

// Send answers the request that is owed, once any idle turn in progress has
// ended. It fails with the reason an idle turn failed, if one did.
func (k *keptConn) Send(ctx context.Context, msg *Response) error {
	k.mu.Lock()
	for k.turn != nil {
		turn := k.turn
		k.mu.Unlock()
		select {
		case <-turn:
		case <-ctx.Done():
			return ctx.Err()
		}
		k.mu.Lock()
	}
	k.owed = false
	if k.timer != nil {
		k.timer.Stop()
		k.timer = nil
	}
	err := k.err
	k.mu.Unlock()
	if err != nil {
		return err
	}
	return k.Conn.Send(ctx, msg)
}

// idle is one idle turn: answer the owed request with a ready and receive the
// client's ready, which is owed in its place.
func (k *keptConn) idle() {
	k.mu.Lock()
	if !k.owed || k.turn != nil || k.err != nil {
		k.mu.Unlock()
		return
	}
	k.turn = make(chan struct{})
	k.mu.Unlock()

	msg, err := k.idleTurn()

	k.mu.Lock()
	defer k.mu.Unlock()
	defer func() {
		close(k.turn)
		k.turn = nil
	}()
	if err != nil {
		k.err = err
		k.fail()
		return
	}
	if msg.Quit {
		k.quit()
	}
	k.owe()
}

// idleTurn sends the ready and returns the client's answer to it.
func (k *keptConn) idleTurn() (*Request, error) {
	if err := k.Conn.Send(k.ctx, k.ready()); err != nil {
		return nil, fmt.Errorf("send idle %s: %w", RespReady, err)
	}
	ctx, cancel := context.WithTimeoutCause(k.ctx, k.within, errIdleSilent)
	defer cancel()
	msg, err := k.Conn.Recv(ctx)
	if err != nil {
		if k.ctx.Err() == nil && errors.Is(context.Cause(ctx), errIdleSilent) {
			return nil, fmt.Errorf("%w within %v", errIdleSilent, k.within)
		}
		return nil, fmt.Errorf("idle recv: %w", err)
	}
	if msg.Type != ReqReady {
		return nil, worker.FatalErrorf("idle recv expected %v, got %v", ReqReady, msg.Type)
	}
	return msg, nil
}

// failure is why an idle turn ended the session, if one did.
func (k *keptConn) failure() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

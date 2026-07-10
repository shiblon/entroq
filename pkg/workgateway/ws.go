package workgateway

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/shiblon/entroq"
	"golang.org/x/sync/errgroup"
)

// WSConn carries the protocol over a coder/websocket connection: one JSON
// message per WebSocket frame.
type WSConn struct {
	c *websocket.Conn
}

// NewWSConn wraps a websocket connection as a Conn.
func NewWSConn(c *websocket.Conn) *WSConn { return &WSConn{c: c} }

// Send writes v as one JSON WebSocket message.
func (w *WSConn) Send(ctx context.Context, v any) error { return wsjson.Write(ctx, w.c, v) }

// Recv reads the next JSON WebSocket message into v.
func (w *WSConn) Recv(ctx context.Context, v any) error { return wsjson.Read(ctx, w.c, v) }

// Handler returns the work gateway's HTTP handler. A worker connects to /work,
// the handler upgrades to WebSocket, and one Bridge runs the protocol over it:
// the worker's first message is a register declaring queues and phases, exactly
// as over any other transport. Canceling ctx stops every connection.
//
// Liveness: a connection dropped mid-task surfaces as a Send/Recv error, which
// ends that worker and reclaims its task. An idle connection (blocked claiming)
// is only noticed at shutdown or the next task; a keepalive ping to catch idle
// drops promptly is a follow-up.
func Handler(ctx context.Context, eq *entroq.EntroQ, lease time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", func(rw http.ResponseWriter, r *http.Request) {
		// These are worker clients, not browsers, so origin checks do not apply.
		c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return // Accept already wrote the response
		}
		defer c.CloseNow()

		bridge := NewBridge(NewWSConn(c))
		switch err := bridge.Run(ctx, eq, lease); {
		case err == nil || errors.Is(err, context.Canceled):
			c.Close(websocket.StatusNormalClosure, "")
		default:
			log.Printf("work: connection %s: %v", r.RemoteAddr, err)
			c.Close(websocket.StatusInternalError, "worker error")
		}
	})
	return mux
}

// Serve runs Handler on addr until ctx is done, then shuts the server down.
func Serve(ctx context.Context, addr string, eq *entroq.EntroQ, lease time.Duration) error {
	srv := &http.Server{Addr: addr, Handler: Handler(ctx, eq, lease)}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-gctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	})
	g.Go(func() error {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	return g.Wait()
}

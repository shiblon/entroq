package workgateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
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

// Server serves the work gateway over WebSocket. A worker connects to /work
// and declares its registration in the URL query string (?queue=... repeated,
// plus optional maxAttempts=N, maxClaims=N, takeDocs=1, work=1, success=1,
// dependency=1, errorQueue=Q, retryDelay=D as a Go duration), the same
// connection preamble a pipe worker supplies via flags. Each connection is
// upgraded to WebSocket and runs one Bridge.
//
// Like http.Server, it tracks its connections so Shutdown can drain them and
// Close can stop them.
type Server struct {
	eq            *entroq.EntroQ
	lease         time.Duration
	entroqTimeout time.Duration
	mux           *http.ServeMux

	mu     sync.Mutex
	closed bool
	conns  map[*Bridge]context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer returns a Server whose connections work against eq with the given
// lease and EntroQ timeout (see WithLease and WithEntroQTimeout).
func NewServer(eq *entroq.EntroQ, lease, entroqTimeout time.Duration) *Server {
	s := &Server{
		eq:            eq,
		lease:         lease,
		entroqTimeout: entroqTimeout,
		mux:           http.NewServeMux(),
		conns:         make(map[*Bridge]context.CancelFunc),
	}
	s.mux.HandleFunc("/work", s.serveWork)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(rw, r) }

// Shutdown drains every connection, as Bridge.Shutdown does, and refuses new
// ones. It returns once all have ended; if ctx ends first, it stops the rest
// as Close does and returns ctx.Err().
func (s *Server) Shutdown(ctx context.Context) error {
	for _, b := range s.closeAll() {
		go b.Shutdown(ctx)
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.Close()
		return ctx.Err()
	}
}

// closeAll refuses new connections and returns the bridges now open.
func (s *Server) closeAll() []*Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	bridges := make([]*Bridge, 0, len(s.conns))
	for b := range s.conns {
		bridges = append(bridges, b)
	}
	return bridges
}

// Close stops every connection at once and refuses new ones. Their tasks are
// left to their leases.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, cancel := range s.conns {
		cancel()
	}
}

// join registers a connection's bridge, or refuses once the server is closed.
func (s *Server) join(b *Bridge, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[b] = cancel
	s.wg.Add(1)
	return true
}

func (s *Server) leave(b *Bridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, b)
	s.wg.Done()
}

func (s *Server) serveWork(rw http.ResponseWriter, r *http.Request) {
	maxAttempts, err := queryInt32(r, "maxAttempts")
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	maxClaims, err := queryInt32(r, "maxClaims")
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var retryDelay time.Duration
	if d := r.URL.Query().Get("retryDelay"); d != "" {
		if retryDelay, err = time.ParseDuration(d); err != nil {
			http.Error(rw, fmt.Sprintf("work gateway: bad retryDelay %q: %v", d, err), http.StatusBadRequest)
			return
		}
	}
	cfg := Config{
		Queues:      r.URL.Query()["queue"],
		ErrorQueue:  r.URL.Query().Get("errorQueue"),
		RetryDelay:  retryDelay,
		MaxAttempts: maxAttempts,
		MaxClaims:   maxClaims,
		TakeDocs:    queryBool(r, "takeDocs"),
		Work:        queryBool(r, "work"),
		Success:     queryBool(r, "success"),
		Dependency:  queryBool(r, "dependency"),
	}
	// Validate the registration before upgrading, so a misconfigured worker
	// gets a plain 400 instead of a successful upgrade followed by an immediate
	// close. Bridge.Run enforces the same invariants once connected.
	if len(cfg.Queues) == 0 {
		http.Error(rw, "work gateway: at least one queue is required", http.StatusBadRequest)
		return
	}
	if !cfg.Work {
		http.Error(rw, "work gateway: work=1 is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A Bridge does not touch its Conn until Run, so it can be registered
	// before the upgrade: a server that is closing refuses with a plain 503.
	conn := &WSConn{}
	bridge := NewBridge(conn, WithConfig(cfg), WithLease(s.lease), WithEntroQTimeout(s.entroqTimeout))
	if !s.join(bridge, cancel) {
		http.Error(rw, "work gateway: shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.leave(bridge)

	// These are worker clients, not browsers, so origin checks do not apply.
	c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept already wrote the response
	}
	defer c.CloseNow()
	conn.c = c

	switch err := bridge.Run(ctx, s.eq); {
	case err == nil || errors.Is(err, context.Canceled):
		c.Close(websocket.StatusNormalClosure, "")
	default:
		code, reason := websocket.StatusInternalError, "worker error"
		if ee, ok := AsExit(err); ok {
			code, reason = closeCodeForClass(ee.Class), ee.Class.String()
		}
		log.Printf("work: connection %s: %v", r.RemoteAddr, err)
		c.Close(code, reason)
	}
}

// closeCodeForClass maps a gateway ExitClass to the WebSocket close code a worker
// client reads to decide retry-vs-stop, drawn from the standard close-code
// registry: transient -> try-again-later, caller fault -> policy violation,
// gateway fault -> internal error. ExitOK never reaches here (it is the
// normal-closure path above).
func closeCodeForClass(c ExitClass) websocket.StatusCode {
	switch c {
	case ExitTransient:
		return websocket.StatusTryAgainLater
	case ExitCaller:
		return websocket.StatusPolicyViolation
	default:
		return websocket.StatusInternalError
	}
}

// queryInt32 parses an optional int32 query parameter, defaulting to 0 when
// absent.
func queryInt32(r *http.Request, key string) (int32, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("bad %s %q: %w", key, s, err)
	}
	return int32(n), nil
}

// queryBool reports whether a boolean query parameter is present and truthy
// (e.g. takeDocs=1 or work=true). An absent or unparseable value is false.
func queryBool(r *http.Request, key string) bool {
	b, _ := strconv.ParseBool(r.URL.Query().Get(key))
	return b
}

// Serve runs s on addr until ctx is done, then closes it: connections still
// open are stopped at once, so call s.Shutdown first to drain them.
func Serve(ctx context.Context, addr string, s *Server) error {
	srv := &http.Server{Addr: addr, Handler: s}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-gctx.Done()
		s.Close()
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

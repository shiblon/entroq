package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/shiblon/entroq"
)

// Handler serves work gateway sessions over HTTP. One request is one exchange:
// a client posts a Request and gets the Response back.
//
// It is the client-driven protocol over the transport that suits it. A POST
// whose answer may take a while is a POST; nothing here is long-polling as a
// workaround, and nothing needs a duplex connection, because the gateway never
// speaks unprompted.
//
// WHAT A SESSION IS, here: two unbuffered channels and a cancel. Serve runs on
// a goroutine of its own and talks to the channels through a ChannelConn; a
// request handler writes the decoded Request to one and reads the Response off
// the other. The handler does not know what phase the session is in and does
// not need to, because the gateway takes the turns.
//
// Sessions live in a map keyed by the id the gateway minted, which this learns
// from the check -- the very message it must read in order to answer the config
// request. So a session is registered before the client can send anything that
// would need to find it.
type Handler struct {
	eq      entroq.Client
	options []Option

	// ctx outlives any one request, because a session does. A request's own
	// context ends when its reply is written, which would end the session with
	// it.
	ctx context.Context

	mu       sync.Mutex
	sessions map[string]*httpSession
}

// httpSession is a transport's half of one conn: the ends the gateway does not
// hold, and the way to stop it.
type httpSession struct {
	req    chan *Request
	resp   chan *Response
	cancel context.CancelFunc

	// done closes when Serve has returned, so a session that ended before it
	// was registered can still be taken back out of the map.
	done chan struct{}

	// id is the session the gateway minted, learned from the check. Guarded by
	// the Handler's mutex, because the goroutine that ends a session reads it
	// while the request that opened one writes it.
	id string
}

// NewHandler makes a Handler that serves sessions against eq until ctx ends.
//
// ctx is the SERVICE's lifetime. Every session runs on a child of it, so
// shutting the service down ends them all, and no session is at the mercy of
// the request that happened to start it.
func NewHandler(ctx context.Context, eq entroq.Client, options ...Option) *Handler {
	return &Handler{
		eq:       eq,
		options:  options,
		ctx:      ctx,
		sessions: make(map[string]*httpSession),
	}
}

// ServeHTTP carries one exchange.
//
// Decoding happens here rather than in the conn, which is the reason this
// transport can work at all: the handler has to see the type and the session id
// to know which session the body belongs to, or whether it starts one.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "the work gateway speaks POST", http.StatusMethodNotAllowed)
		return
	}
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}

	var (
		resp *Response
		err  error
	)
	if req.Type == ReqConfig {
		resp, err = h.open(r.Context(), &req)
	} else {
		resp, err = h.exchange(r.Context(), &req)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("work gateway: write response: %v", err)
	}
}

// errNoSession is what a request naming a session this handler does not hold
// gets. A client answers it by starting over with a config, which costs it a
// claim it had not yet done anything with.
var errNoSession = errors.New("unknown session")

// open starts a session and answers its config request.
//
// The session is registered under the id the check reports, which is why this
// reads the answer before returning it: the client cannot send a second request
// until it has this reply, so nothing can arrive for a session that is not yet
// in the map.
func (h *Handler) open(reqCtx context.Context, req *Request) (*Response, error) {
	ctx, cancel := context.WithCancel(h.ctx)
	s := &httpSession{
		req:    make(chan *Request),
		resp:   make(chan *Response),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	conn := NewChannelConn(s.req, s.resp)

	go func() {
		defer cancel()
		defer close(s.done)
		if err := Serve(ctx, h.eq, conn, h.options...); err != nil {
			log.Printf("work gateway: session ended: %v", err)
		}
		h.forget(s)
	}()

	resp, err := s.exchange(reqCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	// A refused config carries no session, so there is nothing to remember: the
	// goroutine is already on its way out.
	if resp.Session == "" {
		return resp, nil
	}

	h.mu.Lock()
	s.id = resp.Session
	h.sessions[s.id] = s
	h.mu.Unlock()

	// It may have ended between the check and that registration, in which case
	// its own cleanup ran while there was nothing yet to clean. Checking AFTER
	// registering is what makes the two orders equivalent.
	select {
	case <-s.done:
		h.forget(s)
	default:
	}
	return resp, nil
}

// exchange hands a request to the session it names and returns the answer.
func (h *Handler) exchange(reqCtx context.Context, req *Request) (*Response, error) {
	h.mu.Lock()
	s := h.sessions[req.Session]
	h.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("%q: %w", req.Session, errNoSession)
	}
	return s.exchange(reqCtx, req)
}

// forget drops a session from the map once its Serve has returned, so a late
// request is told the session is unknown rather than waiting on channels
// nobody is reading.
func (h *Handler) forget(s *httpSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.id != "" {
		delete(h.sessions, s.id)
	}
}

// exchange is one turn of the protocol over the session's channels: hand the
// request over, wait for the answer.
//
// Both halves watch the request's context, so a client that gives up releases
// this handler. They do NOT cancel the session: the gateway is mid-turn and
// owns the task, and a client that hangs up mid-request is expected to come
// back for the same answer rather than to have lost its work.
func (s *httpSession) exchange(reqCtx context.Context, req *Request) (*Response, error) {
	select {
	case s.req <- req:
	case <-reqCtx.Done():
		return nil, fmt.Errorf("forwarding %q: %w", req.Type, reqCtx.Err())
	}
	select {
	case resp := <-s.resp:
		return resp, nil
	case <-s.done:
		// The session ended without answering, which a well-behaved gateway
		// does not do -- it acknowledges a quit first. Answering rather than
		// waiting out the client's patience is the point: a request that never
		// completes is the worst failure a transport can have.
		return nil, fmt.Errorf("session ended while answering %q: %w", req.Type, errNoSession)
	case <-reqCtx.Done():
		return nil, fmt.Errorf("awaiting answer to %q: %w", req.Type, reqCtx.Err())
	}
}

// Close ends every session this handler holds, and is what a service calls on
// its way down. Cancelling the context given to NewHandler does the same.
func (h *Handler) Close() {
	h.mu.Lock()
	sessions := make([]*httpSession, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.sessions = make(map[string]*httpSession)
	h.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
	}
}

// Sessions reports how many sessions this handler is holding, for a reaper or a
// metric to look at.
func (h *Handler) Sessions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// fail answers a request this handler could not carry.
//
// A session it does not know is the one worth distinguishing: it means start
// over, not try again, so it gets 404 rather than a 5xx a client would retry
// into the same wall.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSession):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client stopped waiting, so there is nobody to tell. 499 is
		// nginx's for exactly this and costs nothing if it goes nowhere.
		w.WriteHeader(499)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

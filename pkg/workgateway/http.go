package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// httpSession is a transport's half of one conn: the ends the gateway does not
// hold, and the way to stop it.
type httpSession struct {
	req    chan *Request
	resp   chan *Response
	conn   *ChannelConn
	cancel context.CancelFunc

	// done closes when serve has returned, so a session that ended before it
	// was registered can still be taken back out of the map.
	done chan struct{}

	// id is the session the gateway minted, learned from the ready. Guarded by
	// the Server's mutex, because the goroutine that ends a session reads it
	// while the request that opened one writes it.
	id string
}

// ServeHTTP carries one exchange.
//
// Decoding happens here rather than in the conn, which is the reason this
// transport can work at all: the handler has to see the type and the session id
// to know which session the body belongs to, or whether it starts one.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		refuse(w, http.StatusMethodNotAllowed, ExitCaller.String(), "the work gateway speaks POST")
		return
	}
	// The same bound the stream transport frames by, so a client's message
	// either fits on both transports or on neither. MaxBytesReader hangs up
	// as well as erroring, so a client still writing a huge body is stopped
	// rather than left to finish sending what nobody will read.
	if s.maxFrameBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxFrameBytes))
	}

	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuse(w, http.StatusRequestEntityTooLarge, ExitCaller.String(), fmt.Sprintf("request over %d bytes", tooLarge.Limit))
			return
		}
		refuse(w, http.StatusBadRequest, ExitCaller.String(), fmt.Sprintf("decode request: %v", err))
		return
	}

	var (
		resp *Response
		err  error
	)
	if req.Type == ReqConfig {
		resp, err = s.open(r.Context(), &req)
	} else {
		resp, err = s.exchange(r.Context(), &req)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	answer(w, statusOf(resp), resp)
}

// classGone is the class of an error for a request naming a session this
// server does not hold. It is not an ExitClass, because nothing exits with it:
// the client starts over with a config, at once.
const classGone = "gone"

// statusOf is the HTTP status for resp: 200, or for an error the status its
// class calls for.
func statusOf(resp *Response) int {
	if resp.Type != RespError {
		return http.StatusOK
	}
	switch resp.Class {
	case ExitCaller.String():
		return http.StatusBadRequest
	case ExitTransient.String():
		return http.StatusServiceUnavailable
	case classGone:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// answer writes resp as the JSON body of a response with status.
func answer(w http.ResponseWriter, status int, resp *Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("work gateway: write response: %v", err)
	}
}

// refuse answers a request with an error of class, and status.
func refuse(w http.ResponseWriter, status int, class, message string) {
	answer(w, status, &Response{Type: RespError, Class: class, Message: message})
}

// errNoSession is what a request naming a session this server does not hold
// gets. A client answers it by starting over with a config, which costs it a
// claim it had not yet done anything with.
var errNoSession = errors.New("unknown session")

// open starts a session and answers its config request.
//
// The session is registered under the id the ready reports, which is why this
// reads the answer before returning it: the client cannot send a second request
// until it has this reply, so nothing can arrive for a session that is not yet
// in the map.
func (s *Server) open(reqCtx context.Context, req *Request) (*Response, error) {
	ctx, cancel := context.WithCancel(s.ctx)
	hs := &httpSession{
		req:    make(chan *Request),
		resp:   make(chan *Response),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	hs.conn = NewChannelConn(hs.req, hs.resp)

	s.enter()
	go func() {
		defer s.leave()
		defer cancel()
		defer close(hs.done)
		if err := s.serve(ctx, hs.conn); err != nil {
			log.Printf("work gateway: session ended: %v", err)
		}
		s.forget(hs)
	}()

	resp, err := hs.exchange(reqCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	// A refused config carries no session, so there is nothing to remember: the
	// goroutine is already on its way out.
	if resp.Session == "" {
		return resp, nil
	}

	s.mu.Lock()
	hs.id = resp.Session
	s.sessions[hs.id] = hs
	s.mu.Unlock()

	// It may have ended between the ready and that registration, in which case
	// its own cleanup ran while there was nothing yet to clean. Checking AFTER
	// registering is what makes the two orders equivalent.
	select {
	case <-hs.done:
		s.forget(hs)
	default:
	}
	return resp, nil
}

// exchange hands a request to the session it names and returns the answer.
func (s *Server) exchange(reqCtx context.Context, req *Request) (*Response, error) {
	s.mu.Lock()
	hs := s.sessions[req.Session]
	s.mu.Unlock()
	if hs == nil {
		return nil, fmt.Errorf("%q: %w", req.Session, errNoSession)
	}
	return hs.exchange(reqCtx, req)
}

// forget drops a session from the map once its serve has returned, so a late
// request is told the session is unknown rather than waiting on channels
// nobody is reading.
func (s *Server) forget(hs *httpSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hs.id != "" {
		delete(s.sessions, hs.id)
	}
}

// exchange is one turn of the protocol over the session's channels: hand the
// request over, wait for the answer.
//
// Both halves watch the request's context, so a client that gives up releases
// this handler. Giving up before the gateway took the request changes nothing.
// Giving up after it is a hang-up: the gateway is acting on a request whose
// answer has nowhere to go, so the session stops claiming and ends.
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
		s.conn.HangUp()
		return nil, fmt.Errorf("awaiting answer to %q: %w", req.Type, reqCtx.Err())
	}
}

// fail answers a request this server could not carry.
//
// A session it does not know is the one worth distinguishing: it means start
// over, not try again, so it is gone, 404, rather than a 5xx a client would
// retry into the same wall.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSession):
		refuse(w, http.StatusNotFound, classGone, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client stopped waiting, so there is nobody to tell. 499 is
		// nginx's for exactly this and costs nothing if it goes nowhere.
		w.WriteHeader(499)
	default:
		refuse(w, http.StatusInternalServerError, ExitGateway.String(), err.Error())
	}
}

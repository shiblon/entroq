package workgateway

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/shiblon/entroq"
)

// Server serves work gateway sessions: over HTTP as an http.Handler, or one at
// a time over a Conn with ServeConn. Like http.Server, it tracks what it serves,
// so Shutdown drains every session and Close stops them all.
//
// Over HTTP one request is one exchange: a client posts a Request and gets the
// Response back. It is the client-driven protocol over the transport that suits
// it. A POST whose answer may take a while is a POST; nothing here is
// long-polling as a workaround, and nothing needs a duplex connection, because
// the gateway never speaks unprompted.
//
// WHAT AN HTTP SESSION IS, here: two unbuffered channels and a cancel. A session
// runs on a goroutine of its own and talks to the channels through a
// ChannelConn; a request handler writes the decoded Request to one and reads
// the Response off the other. The handler does not know what phase the session
// is in and does not need to, because the gateway takes the turns.
//
// HTTP sessions live in a map keyed by the id the gateway minted, which this
// learns from the check -- the very message it must read in order to answer
// the config request. So a session is registered before the client can send
// anything that would need to find it.
type Server struct {
	eq            entroq.Client
	options       []Option
	maxFrameBytes int

	// ctx outlives any one request, because an HTTP session does. A request's
	// own context ends when its reply is written, which would end the session
	// with it.
	ctx context.Context

	mu       sync.Mutex
	sessions map[string]*httpSession

	// gateways are the sessions past their config, for drain and Close to
	// reach. draining refuses any that would join: once set, a config is
	// answered with a quit.
	gateways map[*Gateway]struct{}
	draining bool

	// live counts sessions being served, past their config or not, and ended
	// closes when one returns, for Shutdown to look again.
	live  int
	ended chan struct{}
}

// NewServer makes a Server that serves sessions against eq.
//
// ctx is the SERVICE's lifetime for HTTP sessions. Every one runs on a child of
// it, so cancelling it ends them all, and no session is at the mercy of the
// request that happened to start it.
func NewServer(ctx context.Context, eq entroq.Client, options ...Option) *Server {
	return &Server{
		eq:            eq,
		options:       options,
		maxFrameBytes: resolveOptions(options).maxFrameBytes,
		ctx:           ctx,
		sessions:      make(map[string]*httpSession),
		gateways:      make(map[*Gateway]struct{}),
	}
}

// ServeConn runs one client's session over conn, from its config message until
// whatever ends it, and blocks for the life of the session. Cancelling ctx ends
// it at once, as Close does.
func (s *Server) ServeConn(ctx context.Context, conn Conn) error {
	s.enter()
	defer s.leave()
	return s.serve(ctx, conn)
}

// serve runs one session over conn, for either transport. The caller counts it
// as live (enter, leave) for as long as it runs.
//
// A transport calls this on a context of the TRANSPORT's lifetime rather than
// one request's, and keeps only the conn and that context's cancel. It needs
// nothing else: a session id arrives in the check, which the transport must
// read in order to answer the config request anyway, and which therefore
// reaches it before the client can send anything further.
//
// A registration this gateway will not serve is ANSWERED rather than returned,
// so the error here is for whoever runs the gateway. A client learns from the
// message.
func (s *Server) serve(ctx context.Context, conn Conn) error {
	if s.eq == nil || conn == nil {
		return fmt.Errorf("work gateway needs a client and a conn")
	}
	defer conn.Close()

	first, err := conn.Recv(ctx)
	if err != nil {
		return fmt.Errorf("recv config: %w", err)
	}
	if first != nil && first.Quit {
		// Nothing has started, so there is nothing to drain.
		if err := conn.Send(ctx, &Response{Type: RespQuit}); err != nil {
			return fmt.Errorf("send quit: %w", err)
		}
		return nil
	}

	g, err := newGateway(s.eq, first, conn, s.options...)
	if err != nil {
		if sendErr := conn.Send(ctx, &Response{
			Type:    RespError,
			Class:   ExitCaller.String(),
			Message: err.Error(),
		}); sendErr != nil {
			return fmt.Errorf("send refusal (%v): %w", err, sendErr)
		}
		return fmt.Errorf("config refused: %w", err)
	}
	if !s.join(g) {
		// Draining: this session would claim nothing, so it ends here.
		if err := conn.Send(ctx, &Response{Type: RespQuit}); err != nil {
			return fmt.Errorf("send quit: %w", err)
		}
		return nil
	}
	defer s.part(g)

	// The check answers the config request that got us here, and carries the
	// three things a client cannot work out for itself: which session it is,
	// which consumer holds its tasks, and how long it will actually be given
	// for one of them.
	//
	// Sending it here is safe because serve is already the goroutine: the
	// caller is not waiting on this send, it is waiting to read it.
	check := g.newSend(RespCheck, ReqReady)
	check.Claimant = g.claimant
	check.WorkTimeoutS = int32(g.workTimeout / time.Second)
	check.SilenceTimeoutS = int32(g.silenceTimeout() / time.Second)
	if err := conn.Send(ctx, check); err != nil {
		return fmt.Errorf("send check: %w", err)
	}
	return g.Handle(ctx)
}

// join adds g to the sessions drain and Close reach, or reports false if the
// server is draining. Both happen under the lock drain takes, so a session is
// either drained or refused, never missed.
func (s *Server) join(g *Gateway) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.gateways[g] = struct{}{}
	return true
}

// part removes g once its session has ended.
func (s *Server) part(g *Gateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.gateways, g)
}

// enter counts a session as live, before anything else can see it.
func (s *Server) enter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live++
}

// leave counts a session as ended.
func (s *Server) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live--
	if s.ended != nil {
		close(s.ended)
		s.ended = nil
	}
}

// drain asks every session to stop as its client's quit would, and returns at
// once.
func (s *Server) drain() {
	s.mu.Lock()
	s.draining = true
	gateways := slices.Collect(maps.Keys(s.gateways))
	s.mu.Unlock()
	for _, g := range gateways {
		g.drain()
	}
}

// Shutdown drains every session, like http.Server.Shutdown: none claims again,
// a task in hand finishes through every turn it has left, and each last answer
// is a quit, as for its client's quit. A config that arrives meanwhile is
// answered with a quit. It returns once every session has ended, or, if ctx
// ends first, closes those left and returns ctx.Err().
func (s *Server) Shutdown(ctx context.Context) error {
	s.drain()
	if err := s.wait(ctx); err != nil {
		s.Close()
		return err
	}
	return nil
}

// Close ends every session at once, and is what a service calls when it cannot
// wait for a drain. A task in hand goes back the way an abandoned claim always
// does: its lease lapses. A config that arrives afterward is answered with a
// quit. Cancelling the context given to NewServer ends HTTP sessions the same
// way.
func (s *Server) Close() {
	s.mu.Lock()
	s.draining = true
	gateways := slices.Collect(maps.Keys(s.gateways))
	sessions := slices.Collect(maps.Values(s.sessions))
	s.sessions = make(map[string]*httpSession)
	s.mu.Unlock()
	for _, g := range gateways {
		g.Close()
	}
	for _, hs := range sessions {
		hs.cancel()
	}
}

// wait returns once no session is being served, or ctx.Err() if ctx ends first.
func (s *Server) wait(ctx context.Context) error {
	for {
		s.mu.Lock()
		if s.live == 0 {
			s.mu.Unlock()
			return nil
		}
		if s.ended == nil {
			s.ended = make(chan struct{})
		}
		ended := s.ended
		s.mu.Unlock()
		select {
		case <-ended:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Sessions reports how many HTTP sessions this server is holding, for a reaper
// or a metric to look at.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

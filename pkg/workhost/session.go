// Package workhost runs the EntroQ worker loop on behalf of a worker written in
// any language, which speaks a small JSON protocol and never touches EntroQ.
//
// The client drives. Every message it sends is "here is my answer, what next?",
// and every message the host sends back is an instruction. Neither side ever
// sends anything unprompted, so there is no connection event to handle, no rule
// about who speaks first, and nothing that needs a duplex transport.
//
// See docs/workgateway-protocol-2.md for the protocol itself.
package workhost

import (
	"context"
	"errors"
	"sync"
	"time"
)

// A session is one client's conversation with the host: a worker loop running
// against EntroQ, and a client taking turns with it.
//
// TWO RENDEZVOUS AND ONE TOKEN is the whole mechanism. The worker loop offers
// an instruction and waits for the answer; a client request delivers the answer
// to the previous instruction and waits for the next one. Both channels are
// unbuffered, so an instruction is handed to a waiting request and an answer to
// a waiting phase -- never left somewhere for whoever finds it, which is what a
// buffer invites.
//
// The protocol allows one request in flight per session. That is a token here
// rather than a flag, so a second concurrent request is refused outright
// instead of racing the first through shared state.
type session struct {
	id       string
	claimant string

	// turn holds one token. A request takes it for the length of its exchange,
	// so a client that sends two at once gets errConcurrent for the second
	// rather than interleaving with itself.
	turn chan struct{}

	// instr carries an instruction from the worker loop to the request that
	// will answer with it. reply carries a client's answer back to the phase
	// waiting for it.
	instr chan instruction
	reply chan answer

	// closed when the session is over, for whatever reason. Every select in
	// here watches it, so nothing waits on a conversation that has ended.
	done     chan struct{}
	closeErr error
	closeOne sync.Once
}

// errConcurrent is what a second simultaneous request gets. One request at a
// time per session is the protocol; this is not a race to be tolerated but a
// client bug to report.
var errConcurrent = errors.New("workhost: another request is already in flight for this session")

// errGone is what a phase or a request gets once the session has ended.
var errGone = errors.New("workhost: session is over")

func newSession(id, claimant string) *session {
	s := &session{
		id:       id,
		claimant: claimant,
		turn:     make(chan struct{}, 1),
		instr:    make(chan instruction),
		reply:    make(chan answer),
		done:     make(chan struct{}),
	}
	s.turn <- struct{}{}
	return s
}

// close ends the session, recording why. Safe to call repeatedly and from
// anywhere; the first reason is the one kept.
func (s *session) close(err error) {
	s.closeOne.Do(func() {
		s.closeErr = err
		close(s.done)
	})
}

// why reports what ended the session, or nil if it has not.
func (s *session) why() error {
	select {
	case <-s.done:
		if s.closeErr != nil {
			return s.closeErr
		}
		return errGone
	default:
		return nil
	}
}

// ask offers in to the client and returns the client's answer. The worker loop
// calls it, once per phase, and blocks in it for as long as the client takes.
//
// A client that stops asking leaves this blocked, holding a claimed task. That
// is what the session's idle deadline is for: it closes the session, which ends
// the wait and lets the task go back.
func (s *session) ask(ctx context.Context, in instruction) (answer, error) {
	select {
	case s.instr <- in:
	case <-s.done:
		return answer{}, s.why()
	case <-ctx.Done():
		return answer{}, ctx.Err()
	}

	select {
	case a := <-s.reply:
		return a, nil
	case <-s.done:
		return answer{}, s.why()
	case <-ctx.Done():
		return answer{}, ctx.Err()
	}
}

// exchange is one client request: it delivers a's answer to the phase waiting
// for it, then waits for the next instruction to send back.
//
// When nothing is ready within wait it returns a hold instruction, which tells
// the client to ask again. The timeout is on the RECEIVE, so an instruction is
// never taken and then dropped -- the thing a timeout placed after the receive
// would do, and the shape of the race this protocol is replacing.
func (s *session) exchange(ctx context.Context, a answer, wait time.Duration) (instruction, error) {
	select {
	case <-s.turn:
	default:
		return instruction{}, errConcurrent
	}
	defer func() { s.turn <- struct{}{} }()

	if a.answers {
		select {
		case s.reply <- a:
		case <-s.done:
			return instruction{}, s.why()
		case <-ctx.Done():
			return instruction{}, ctx.Err()
		}
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case in := <-s.instr:
		return in, nil
	case <-timer.C:
		return hold(), nil
	case <-s.done:
		return instruction{}, s.why()
	case <-ctx.Done():
		return instruction{}, ctx.Err()
	}
}

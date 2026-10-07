package workhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestSessionTakesTurns covers the ordinary alternation: a phase offers an
// instruction, a request collects it and answers, the next phase sees that
// answer.
func TestSessionTakesTurns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newSession("sess-1", "host/1")

	var got answer
	var askErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, askErr = s.ask(ctx, instruction{Type: insDoWork})
	}()

	in, err := s.exchange(ctx, answer{Type: msgConfig}, time.Second)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if in.Type != insDoWork {
		t.Errorf("instruction = %q, want %q", in.Type, insDoWork)
	}

	// The phase is still waiting for the answer, which the NEXT request carries.
	if _, err := s.exchange(ctx, answer{Type: msgResult, Outcome: outcomeOK, answers: true}, time.Second); err != nil && !errors.Is(err, errGone) {
		// No further instruction is coming, so this one holds; either is fine.
		t.Logf("second exchange: %v", err)
	}

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the phase never received its answer")
	}
	if askErr != nil {
		t.Fatalf("ask: %v", askErr)
	}
	if got.Type != msgResult || got.Outcome != outcomeOK {
		t.Errorf("answer = %+v, want a result with outcome %q", got, outcomeOK)
	}
}

// TestSessionHoldLosesNothing is the property that replaces protocol 1's race.
//
// A request that waits and finds nothing returns a hold, and the instruction the
// worker offers a moment later must still be delivered to the NEXT request. The
// timeout is on the receive, so it cannot take an instruction and then drop it --
// which is exactly what protocol 1 did by checking its reply channel after the
// wait had already ended, with no lock against the goroutine delivering into it.
func TestSessionHoldLosesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newSession("sess-2", "host/1")

	// Nothing is on offer yet, so this holds.
	in, err := s.exchange(ctx, answer{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if in.Type != insHold {
		t.Fatalf("instruction = %q, want %q", in.Type, insHold)
	}

	// The worker offers work just after the hold went out.
	offered := make(chan struct{})
	go func() {
		defer close(offered)
		if _, err := s.ask(ctx, instruction{Type: insDoWork, Session: "proof"}); err != nil {
			t.Errorf("ask: %v", err)
		}
	}()

	// The next request must get that instruction, not another hold.
	in, err = s.exchange(ctx, answer{}, 2*time.Second)
	if err != nil {
		t.Fatalf("exchange after hold: %v", err)
	}
	if in.Type != insDoWork || in.Session != "proof" {
		t.Errorf("instruction after a hold = %+v, want the offered doWork", in)
	}

	// Let the phase finish so the goroutine does not leak into the next test.
	if _, err := s.exchange(ctx, answer{Type: msgResult, answers: true}, 20*time.Millisecond); err != nil {
		t.Logf("final exchange: %v", err)
	}
	select {
	case <-offered:
	case <-ctx.Done():
		t.Fatal("the phase never completed")
	}
}

// TestSessionRefusesConcurrentRequests covers the one-request-at-a-time rule.
// It is the protocol, so a second simultaneous request is a client bug to
// report rather than a race to absorb.
func TestSessionRefusesConcurrentRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newSession("sess-3", "host/1")

	// The first request parks, waiting for an instruction that never comes.
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		if _, err := s.exchange(ctx, answer{}, 500*time.Millisecond); err != nil {
			t.Errorf("first exchange: %v", err)
		}
	}()

	// Give it time to take the turn, then try to overlap it.
	time.Sleep(50 * time.Millisecond)
	if _, err := s.exchange(ctx, answer{}, time.Second); !errors.Is(err, errConcurrent) {
		t.Errorf("overlapping request: got %v, want errConcurrent", err)
	}

	<-parked
	// The turn comes back, so the session is usable again.
	if _, err := s.exchange(ctx, answer{}, 20*time.Millisecond); err != nil {
		t.Errorf("exchange after the first finished: %v", err)
	}
}

// TestSessionCloseUnblocksBothSides covers the only way a conversation ends
// badly: the session closes, and nothing is left waiting on it. A client that
// stops asking leaves a phase blocked holding a claimed task, so this is what
// lets that task go back.
func TestSessionCloseUnblocksBothSides(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("a waiting phase", func(t *testing.T) {
		s := newSession("sess-4", "host/1")
		errc := make(chan error, 1)
		go func() {
			_, err := s.ask(ctx, instruction{Type: insDoWork})
			errc <- err
		}()
		time.Sleep(20 * time.Millisecond)
		s.close(errors.New("idle too long"))
		select {
		case err := <-errc:
			if err == nil || err.Error() != "idle too long" {
				t.Errorf("ask after close: got %v, want the reason the session closed", err)
			}
		case <-ctx.Done():
			t.Fatal("a waiting phase was not released by closing the session")
		}
	})

	t.Run("a waiting request", func(t *testing.T) {
		s := newSession("sess-5", "host/1")
		errc := make(chan error, 1)
		go func() {
			_, err := s.exchange(ctx, answer{}, time.Minute)
			errc <- err
		}()
		time.Sleep(20 * time.Millisecond)
		s.close(nil)
		select {
		case err := <-errc:
			if !errors.Is(err, errGone) {
				t.Errorf("exchange after close: got %v, want errGone", err)
			}
		case <-ctx.Done():
			t.Fatal("a waiting request was not released by closing the session")
		}
	})
}

// TestSessionCloseIsIdempotent keeps the first reason, since that is the one
// that explains what happened.
func TestSessionCloseIsIdempotent(t *testing.T) {
	s := newSession("sess-6", "host/1")
	first := errors.New("first")
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.close(first)
		}()
	}
	wg.Wait()
	s.close(errors.New("second"))
	if got := s.why(); got != first {
		t.Errorf("why() = %v, want the first reason %v", got, first)
	}
}

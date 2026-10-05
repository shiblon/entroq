package eqpg

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
)

// TestInterruptedCarriesBothReasons covers what a modification must report
// when the caller stops caring mid-query. lib/pq surfaces the server's own
// "canceling statement due to user request" rather than the context's error,
// so interrupted wraps the context's reason around it: without that, a
// cancellation would read to the caller as a database failure.
//
// The behavior under test is the mapping, not a query: interrupted wraps
// whatever error it is handed once the context is done. So this asserts it
// directly, with no database, rather than arranging a slow query and a timer
// to provoke it.
//
// IsCanceled and IsTimeout are deliberately separate: a caller that gave up
// on a deadline is distinguishable from one that was told to stop. The
// expectations below are derived rather than written per case, so a new case
// cannot be given a convenient answer.
func TestInterruptedCarriesBothReasons(t *testing.T) {
	// The shape lib/pq reports when the server kills a statement for us.
	serverCancel := &pq.Error{Code: "57014", Message: "canceling statement due to user request"}
	dbFailure := errors.New("constraint violated")

	for name, tc := range map[string]struct {
		ctxErr error // the context's reason, nil for a live context
		err    error // what the database layer returned
	}{
		"server cancel, caller canceled":  {ctxErr: context.Canceled, err: serverCancel},
		"server cancel, caller timed out": {ctxErr: context.DeadlineExceeded, err: serverCancel},
		"real failure, live context":      {ctxErr: nil, err: dbFailure},
		"real failure, caller canceled":   {ctxErr: context.Canceled, err: dbFailure},
		"context error needs no re-wrap":  {ctxErr: context.Canceled, err: context.Canceled},
		"no error stays no error":         {ctxErr: context.Canceled, err: nil},
		"live context leaves nil alone":   {ctxErr: nil, err: nil},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			switch tc.ctxErr {
			case context.Canceled:
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = canceled
			case context.DeadlineExceeded:
				// Already past, so Err is set without waiting.
				expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				ctx = expired
			}
			if ctx.Err() != tc.ctxErr {
				t.Fatalf("context Err is %v, want %v: the case is not set up as intended", ctx.Err(), tc.ctxErr)
			}

			got := interrupted(ctx, tc.err)

			switch {
			case tc.err == nil:
				if got != nil {
					t.Errorf("interrupted(nil) = %v, want nil however the context ended", got)
				}
				return
			case tc.ctxErr == nil:
				if !errors.Is(got, tc.err) {
					t.Errorf("interrupted = %v, want the original error untouched on a live context", got)
				}
				if entroq.IsCanceled(got) || entroq.IsTimeout(got) {
					t.Errorf("interrupted = %v reads as canceled or timed out on a LIVE context", got)
				}
				return
			}

			// The context ended and there is an error: the result must carry
			// both reasons. The context's, so the caller can tell it was its
			// own doing; the original, so an operator can still see what the
			// database said.
			if !errors.Is(got, tc.ctxErr) {
				t.Errorf("interrupted = %v, which does not carry the context's %v", got, tc.ctxErr)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("interrupted = %v, which no longer wraps the original %v", got, tc.err)
			}
			// And it must answer the matching predicate, not the other one.
			wantCanceled := errors.Is(tc.ctxErr, context.Canceled)
			if entroq.IsCanceled(got) != wantCanceled {
				t.Errorf("IsCanceled(%v) = %v, want %v", got, entroq.IsCanceled(got), wantCanceled)
			}
			if entroq.IsTimeout(got) != !wantCanceled {
				t.Errorf("IsTimeout(%v) = %v, want %v", got, entroq.IsTimeout(got), !wantCanceled)
			}
		})
	}

	// Guard the premise: a bare pq error of this shape is not a cancellation
	// on its own, so the wrapping is doing the work rather than the error
	// arriving already canceled.
	if wrapped := fmt.Errorf("pg modify: %w", serverCancel); entroq.IsCanceled(wrapped) || entroq.IsTimeout(wrapped) {
		t.Error("a server-cancel pq error reads as canceled or timed out on its own, so this test proves nothing")
	}
}

package workgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// TestDispositionMapsEveryOutcome locks the one mapping this protocol exists
// to provide: a client names a disposition and the worker performs it.
//
// The worker is what counts an attempt, quarantines at the ceiling and sets the
// backoff arrival, so a client must never do any of that itself with a raw
// modification. That is the bug AGENTS.md names: retry and quarantine are one
// decision, taken at the moment of failure.
func TestDispositionMapsEveryOutcome(t *testing.T) {
	g := &Gateway{config: &Config{}}

	t.Run("ok is no disposition at all", func(t *testing.T) {
		if err := g.disposition(OutcomeOK, nil); err != nil {
			t.Errorf("disposition(ok) = %v, want nil", err)
		}
	})

	t.Run("retry", func(t *testing.T) {
		err := g.disposition(OutcomeRetry, &TaskError{Message: "upstream is busy"})
		re, ok := worker.AsRetry(err)
		if !ok {
			t.Fatalf("disposition(retry) = %T (%v), want a *worker.RetryError", err, err)
		}
		if !strings.Contains(re.Error(), "upstream is busy") {
			t.Errorf("retry does not carry the client's reason: %v", re)
		}
	})

	t.Run("move", func(t *testing.T) {
		err := g.disposition(OutcomeMove, &TaskError{Message: "poison", Queue: "dead"})
		me, ok := worker.AsMove(err)
		if !ok {
			t.Fatalf("disposition(move) = %T (%v), want a *worker.MoveError", err, err)
		}
		if !strings.Contains(me.Error(), "poison") {
			t.Errorf("move does not carry the client's reason: %v", me)
		}
	})

	t.Run("fatal", func(t *testing.T) {
		err := g.disposition(OutcomeFatal, &TaskError{Message: "config is wrong"})
		if _, ok := worker.AsFatal(err); !ok {
			t.Fatalf("disposition(fatal) = %T (%v), want a *worker.FatalError", err, err)
		}
	})

	t.Run("error is deliberately not a sentinel", func(t *testing.T) {
		// An unclassified failure must reach runOne as a plain error, which is
		// what makes it quarantine the task when this claim was its last and
		// then stop. A sentinel here would skip that.
		err := g.disposition(OutcomeError, &TaskError{Message: "surprise"})
		if err == nil {
			t.Fatal("disposition(error) = nil, want an error")
		}
		for name, is := range map[string]bool{
			"retry": isRetry(err), "move": isMove(err), "fatal": isFatal(err),
		} {
			if is {
				t.Errorf("disposition(error) is a %s sentinel; it must be a plain error", name)
			}
		}
	})

	t.Run("an unknown outcome is the client's fault", func(t *testing.T) {
		err := g.disposition(Outcome("sideways"), nil)
		if _, ok := worker.AsFatal(err); !ok {
			t.Errorf("disposition(unknown) = %T (%v), want a *worker.FatalError", err, err)
		}
		if !strings.Contains(err.Error(), "sideways") {
			t.Errorf("refusal does not name the outcome it did not understand: %v", err)
		}
	})

	t.Run("a missing reason still reads", func(t *testing.T) {
		// A client may report an outcome with no error block at all; the
		// message must not be empty or a log says only that something failed.
		err := g.disposition(OutcomeRetry, nil)
		if err == nil || err.Error() == "" {
			t.Fatalf("disposition(retry, nil) = %v, want a readable error", err)
		}
	})
}

// TestRetryDelayComesFromConfig checks that the session's base retry delay
// reaches the worker, since it is a deployment's choice and the only way a
// client influences backoff.
func TestRetryDelayComesFromConfig(t *testing.T) {
	plain := &Gateway{config: &Config{}}
	if got := plain.retryDelay(); got != 0 {
		t.Errorf("retryDelay with no config = %v, want 0 so the worker default stands", got)
	}

	g := &Gateway{config: &Config{RetryDelayS: 45}}
	if got := g.retryDelay(); got != 45*time.Second {
		t.Errorf("retryDelay = %v, want 45s", got)
	}
	// And it rides on the disposition rather than being applied by the client.
	err := g.disposition(OutcomeRetry, &TaskError{Message: "later"})
	if _, ok := worker.AsRetry(err); !ok {
		t.Fatalf("disposition(retry) = %T, want a *worker.RetryError", err)
	}
}

// TestClassifyTellsASupervisorWhatToDo covers the vocabulary a transport maps
// to an exit code or an HTTP status. Getting ExitOK wrong is the costly
// direction: a real failure would report success.
func TestClassifyTellsASupervisorWhatToDo(t *testing.T) {
	for name, tc := range map[string]struct {
		g    *Gateway
		err  error
		want ExitClass
	}{
		"nil is clean":            {&Gateway{}, nil, ExitOK},
		"canceled is clean":       {&Gateway{}, context.Canceled, ExitOK},
		"wrapped cancel is clean": {&Gateway{}, fmt.Errorf("run: %w", context.Canceled), ExitOK},
		"unavailable is transient": {
			&Gateway{}, entroq.Unavailablef("backend down"), ExitTransient,
		},
		"a fatal is the caller's fault": {
			&Gateway{}, worker.FatalErrorf("bad message"), ExitCaller,
		},
		"a surprise is ours": {&Gateway{}, errors.New("surprise"), ExitGateway},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.g.classify(tc.err); got != tc.want {
				t.Errorf("classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	t.Run("a closed session is clean whatever it was doing", func(t *testing.T) {
		// Close is how a transport whose client has gone, or a reaper, ends a
		// session. Whatever the worker was mid-way through reports as an error,
		// and none of it is a fault.
		g := &Gateway{}
		g.Close()
		if got := g.classify(errors.New("surprise")); got != ExitOK {
			t.Errorf("classify after Close = %v, want %v", got, ExitOK)
		}
	})
}

// TestExitCarriesTheClass checks that a transport reads a class rather than
// interpreting a Go error, and that a clean stop stays a nil error.
func TestExitCarriesTheClass(t *testing.T) {
	g := &Gateway{}
	if err := g.exit(nil); err != nil {
		t.Errorf("exit(nil) = %v, want nil", err)
	}
	if err := g.exit(context.Canceled); err != nil {
		t.Errorf("exit(canceled) = %v, want nil: cancellation is a clean stop", err)
	}

	cause := entroq.Unavailablef("backend down")
	err := g.exit(cause)
	ee, ok := AsExit(err)
	if !ok {
		t.Fatalf("exit(%v) = %v, which carries no class", cause, err)
	}
	if ee.Class != ExitTransient {
		t.Errorf("class = %v, want %v", ee.Class, ExitTransient)
	}
	if ee.Class.ExitCode() != 75 {
		t.Errorf("exit code = %d, want 75 (EX_TEMPFAIL)", ee.Class.ExitCode())
	}
	if !errors.Is(err, cause) {
		t.Error("the cause did not survive into the ExitError")
	}
}

func isRetry(err error) bool { _, ok := worker.AsRetry(err); return ok }
func isMove(err error) bool  { _, ok := worker.AsMove(err); return ok }
func isFatal(err error) bool { _, ok := worker.AsFatal(err); return ok }

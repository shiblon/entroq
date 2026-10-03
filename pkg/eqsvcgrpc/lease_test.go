package eqsvcgrpc

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// leaseSvc is the smallest service that can resolve a lease: the bounds and a
// counter to record a clamp against. It needs no backend, because resolving a
// lease touches no storage.
func leaseSvc(t *testing.T, floor, ceiling time.Duration) *QSvc {
	t.Helper()
	counter, err := noop.NewMeterProvider().Meter("test").Int64Counter("clamped")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	return &QSvc{leaseFloor: floor, leaseCeiling: ceiling, leaseClamped: counter}
}

// until reports the lease arg's resolved hold time, for a claim made at now.
// It goes through DocClaim so the test reads the value the way a backend will,
// rather than reaching into the arg.
func until(arg entroq.DocClaimArg, now time.Time) time.Time {
	if arg == nil {
		return now.Add(entroq.DefaultClaimDuration)
	}
	return entroq.NewDocClaim(arg).Until(now)
}

func TestResolveSetLease(t *testing.T) {
	const (
		floor   = 30 * time.Second
		ceiling = time.Hour
	)
	// The shortest hold a named time can produce. Taken from the function the
	// service itself uses, so these cases cannot drift from it.
	minHold := entroq.RenewalDurationFor(floor)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		duration time.Duration
		at       time.Time
		want     time.Duration // hold time from now; 0 means the default
	}{{
		name: "neither given takes the default",
		want: entroq.DefaultClaimDuration,
	}, {
		name:     "a duration in bounds is honored",
		duration: 5 * time.Minute,
		want:     5 * time.Minute,
	}, {
		name:     "a duration below the floor is clamped up",
		duration: time.Second,
		want:     floor,
	}, {
		name:     "a duration above the ceiling is clamped down",
		duration: 24 * time.Hour,
		want:     ceiling,
	}, {
		// The whole point of allowing both: hold until the task's arrival.
		name:     "a time far enough out wins over the duration",
		duration: time.Minute,
		at:       now.Add(10 * time.Minute),
		want:     10 * time.Minute,
	}, {
		// A time wins outright: the two are never weighed against each other,
		// so naming a duration as well changes nothing.
		name:     "a duration is ignored when a time is given",
		duration: time.Minute,
		at:       now.Add(25 * time.Second),
		want:     25 * time.Second,
	}, {
		// Deliberate: a time may reach below the floor, which is what lets a
		// claim expire in step with a part-spent task.
		name: "a time above the minimum hold is honored below the lease floor",
		at:   now.Add(25 * time.Second),
		want: 25 * time.Second,
	}, {
		// The boundary is inclusive: pins the comparison, not just the clamp.
		name: "a time exactly at the minimum hold is honored",
		at:   now.Add(minHold),
		want: minHold,
	}, {
		name:     "a time beyond the ceiling is clamped down",
		duration: time.Minute,
		at:       now.Add(48 * time.Hour),
		want:     ceiling,
	}, {
		// Clamped up to the minimum hold, not down to the absurd duration beside
		// it: a time is bounded on its own terms.
		name:     "a time below the minimum hold is clamped up",
		duration: time.Millisecond,
		at:       now.Add(10 * time.Second),
		want:     minHold,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := leaseSvc(t, floor, ceiling)
			arg, err := s.resolveSetLease(context.Background(), now, tc.duration, tc.at)
			if err != nil {
				t.Fatalf("resolveSetLease: %v", err)
			}
			if got, want := until(arg, now), now.Add(tc.want); !got.Equal(want) {
				t.Errorf("hold until %v, want %v (%v from now)", got, want, tc.want)
			}
		})
	}
}

func TestResolveSetLeaseRefusesPastTime(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s := leaseSvc(t, 30*time.Second, time.Hour)

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{name: "in the past", at: now.Add(-time.Minute)},
		{name: "exactly now", at: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A past time cannot be honored at all, so it is refused rather than
			// quietly replaced by the duration: it is always a caller's mistake.
			if _, err := s.resolveSetLease(context.Background(), now, time.Minute, tc.at); err == nil {
				t.Fatal("resolveSetLease accepted a non-future time, want InvalidArgument")
			} else if code := status.Code(err); code != codes.InvalidArgument {
				t.Errorf("resolveSetLease code = %v, want %v", code, codes.InvalidArgument)
			}
		})
	}
}

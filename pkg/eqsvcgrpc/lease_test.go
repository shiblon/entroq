package eqsvcgrpc

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
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

// claimOf builds the claim a lease arg describes, the way the backend will see
// it. A nil arg means the request named no lease, which entroq.ClaimDocs fills
// in with the default.
func claimOf(arg entroq.DocClaimArg) *entroq.DocClaim {
	if arg == nil {
		return &entroq.DocClaim{Duration: entroq.DefaultClaimDuration}
	}
	return entroq.NewDocClaim(arg)
}

func TestResolveSetLeaseClampsDurations(t *testing.T) {
	const (
		floor   = 30 * time.Second
		ceiling = time.Hour
	)
	for _, tc := range []struct {
		name     string
		duration time.Duration
		want     time.Duration
	}{{
		name: "nothing given takes the default",
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
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := leaseSvc(t, floor, ceiling)
			arg, err := s.resolveSetLease(context.Background(), nil, tc.duration)
			if err != nil {
				t.Fatalf("resolveSetLease: %v", err)
			}
			cq := claimOf(arg)
			if cq.TaskToMatch != nil {
				t.Fatalf("resolved to a match of task %v, want a duration", cq.TaskToMatch)
			}
			if cq.Duration != tc.want {
				t.Errorf("hold for %v, want %v", cq.Duration, tc.want)
			}
		})
	}
}

// TestResolveSetLeaseMatchPassesThrough pins that a matched claim reaches the
// backend untouched, carrying the task and no duration.
//
// The service has no length to clamp here: the request names a task, and the
// hold comes from that task's own arrival, which was bounded when the task was
// claimed. A duration sent alongside is not consulted, so the case below
// passes one that would be clamped on its own and expects it ignored.
func TestResolveSetLeaseMatchPassesThrough(t *testing.T) {
	s := leaseSvc(t, 30*time.Second, time.Hour)
	match := &pb.TaskID{Id: "t", Version: 7, Queue: "/q"}

	// A duration alongside it would have been clamped on its own; with a match
	// present it is not consulted at all.
	arg, err := s.resolveSetLease(context.Background(), match, time.Millisecond)
	if err != nil {
		t.Fatalf("resolveSetLease: %v", err)
	}
	cq := entroq.NewDocClaim(arg)
	if cq.Duration != 0 {
		t.Errorf("a matched claim carries duration %v, want it to carry the task alone", cq.Duration)
	}
	if cq.TaskToMatch == nil {
		t.Fatal("a matched claim carries no task")
	}
	if got, want := *cq.TaskToMatch, (entroq.TaskID{ID: "t", Version: 7, Queue: "/q"}); got != want {
		t.Errorf("matched task is %v, want %v: the version and queue must survive, since the backend needs both", got, want)
	}
}

func TestResolveSetLeaseRefusesUnusableMatch(t *testing.T) {
	s := leaseSvc(t, 30*time.Second, time.Hour)
	for name, match := range map[string]*pb.TaskID{
		"no ID":    {Version: 3, Queue: "/q"},
		"no queue": {Id: "t", Version: 3},
		"neither":  {Version: 3},
	} {
		t.Run(name, func(t *testing.T) {
			// A backend needs the queue to find the task at all, so an
			// unusable reference is refused here rather than failing later as
			// a missing task, which would read as a lost claim instead of a
			// malformed request.
			if _, err := s.resolveSetLease(context.Background(), match, time.Minute); err == nil {
				t.Fatal("resolveSetLease accepted an unusable match, want InvalidArgument")
			} else if code := status.Code(err); code != codes.InvalidArgument {
				t.Errorf("resolveSetLease code = %v, want %v", code, codes.InvalidArgument)
			}
		})
	}
}

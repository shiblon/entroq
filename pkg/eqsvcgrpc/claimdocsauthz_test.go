package eqsvcgrpc

import (
	"context"
	"slices"
	"testing"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// actionsFor returns the actions requested for one namespace, and whether it
// was named at all.
func actionsFor(req *authz.Request, ns string) ([]authz.Action, bool) {
	for _, n := range req.Namespaces {
		if n.Exact == ns {
			return n.Actions, true
		}
	}
	return nil, false
}

// TestClaimDocsAuthzAsksForWhatTheClaimDoes covers what a doc claim must be
// allowed to do, which is more than take ownership: a claim hands back the
// set's docs, and a claim matching a task reads that task.
func TestClaimDocsAuthzAsksForWhatTheClaimDoes(t *testing.T) {
	ctx := context.Background()
	s := &QSvc{} // no authorizer: the request is still built.

	t.Run("a set whose members come back needs Claim and Read", func(t *testing.T) {
		cq := &pb.DocClaim{Claimant: "worker-1"}
		dc := &entroq.DocClaim{Claimant: "worker-1", Sets: []*entroq.DocSetClaim{
			{Namespace: "ns-read", Key: "k"},
		}}
		req := s.claimDocsAuthz(ctx, cq, dc)
		got, ok := actionsFor(req, "ns-read")
		if !ok {
			t.Fatalf("namespace ns-read was not named: %+v", req.Namespaces)
		}
		if !slices.Contains(got, authz.Claim) || !slices.Contains(got, authz.Read) {
			t.Errorf("ns-read: got %v, want both Claim and Read", got)
		}
		if req.ClaimantId != "worker-1" {
			t.Errorf("claimant: got %q, want %q", req.ClaimantId, "worker-1")
		}
	})

	t.Run("a set claimed without its members needs Claim alone", func(t *testing.T) {
		cq := &pb.DocClaim{Claimant: "worker-1"}
		dc := &entroq.DocClaim{Claimant: "worker-1", Sets: []*entroq.DocSetClaim{
			{Namespace: "ns-lock", Key: "k", OmitMembers: true},
		}}
		req := s.claimDocsAuthz(ctx, cq, dc)
		got, ok := actionsFor(req, "ns-lock")
		if !ok {
			t.Fatalf("namespace ns-lock was not named: %+v", req.Namespaces)
		}
		if !slices.Equal(got, []authz.Action{authz.Claim}) {
			t.Errorf("ns-lock: got %v, want Claim alone -- nothing is disclosed", got)
		}
	})

	t.Run("one namespace with both kinds of set needs Read", func(t *testing.T) {
		// The actions are per namespace, so the set that discloses decides.
		cq := &pb.DocClaim{Claimant: "worker-1"}
		dc := &entroq.DocClaim{Claimant: "worker-1", Sets: []*entroq.DocSetClaim{
			{Namespace: "ns-mixed", Key: "quiet", OmitMembers: true},
			{Namespace: "ns-mixed", Key: "loud"},
		}}
		req := s.claimDocsAuthz(ctx, cq, dc)
		if len(req.Namespaces) != 1 {
			t.Fatalf("got %d namespaces, want 1: %+v", len(req.Namespaces), req.Namespaces)
		}
		got, _ := actionsFor(req, "ns-mixed")
		if !slices.Contains(got, authz.Claim) || !slices.Contains(got, authz.Read) {
			t.Errorf("ns-mixed: got %v, want both Claim and Read", got)
		}
		if len(got) != 2 {
			t.Errorf("ns-mixed: got %v, want each action once", got)
		}
	})

	t.Run("a matched task needs Read on its queue", func(t *testing.T) {
		cq := &pb.DocClaim{
			Claimant:    "worker-1",
			TaskToMatch: &pb.TaskID{Id: "t1", Version: 3, Queue: "q-matched"},
		}
		dc := &entroq.DocClaim{Claimant: "worker-1", Sets: []*entroq.DocSetClaim{
			{Namespace: "ns", Key: "k"},
		}}
		req := s.claimDocsAuthz(ctx, cq, dc)
		if len(req.Queues) != 1 {
			t.Fatalf("got %d queues, want the matched task's: %+v", len(req.Queues), req.Queues)
		}
		q := req.Queues[0]
		if q.Exact != "q-matched" {
			t.Errorf("queue: got %q, want %q", q.Exact, "q-matched")
		}
		if !slices.Equal(q.Actions, []authz.Action{authz.Read}) {
			t.Errorf("queue %q: got %v, want Read", q.Exact, q.Actions)
		}
	})

	t.Run("a claim naming no task names no queue", func(t *testing.T) {
		cq := &pb.DocClaim{Claimant: "worker-1", DurationMs: 60000}
		dc := &entroq.DocClaim{Claimant: "worker-1", Sets: []*entroq.DocSetClaim{
			{Namespace: "ns", Key: "k"},
		}}
		req := s.claimDocsAuthz(ctx, cq, dc)
		if len(req.Queues) != 0 {
			t.Errorf("got %+v, want no queue: the claim reads no task", req.Queues)
		}
	})
}

// TestClaimDocsNamesSetsByProtocol: each protocol names what to claim its own
// way. Protocol 1 claims the set namespace and key name; protocol 2 names its
// sets in sets, and a protocol-2 claim with none is refused rather than read
// as protocol 1.
func TestClaimDocsNamesSetsByProtocol(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	declared := func(p string) context.Context {
		return metadata.NewIncomingContext(ctx, metadata.Pairs(version.ProtocolHeader, p))
	}

	if _, err := svc.ClaimDocs(ctx, &pb.ClaimDocsRequest{ClaimQuery: &pb.DocClaim{
		Claimant: "c1", Namespace: "ns", Key: "k1", DurationMs: 60_000,
	}}); err != nil {
		t.Errorf("protocol 1 claim by namespace and key: %v", err)
	}
	_, err = svc.ClaimDocs(declared("2"), &pb.ClaimDocsRequest{ClaimQuery: &pb.DocClaim{
		Claimant: "c1", Namespace: "ns", Key: "k2", DurationMs: 60_000,
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("protocol 2 claim with no sets: got %v, want InvalidArgument", err)
	}
	if _, err := svc.ClaimDocs(declared("2"), &pb.ClaimDocsRequest{ClaimQuery: &pb.DocClaim{
		Claimant: "c1", DurationMs: 60_000,
		Sets: []*pb.DocSetClaim{{Set: &pb.DocID{Namespace: "ns", Ref: &pb.DocID_Key{Key: "k3"}}}},
	}}); err != nil {
		t.Errorf("protocol 2 claim by sets: %v", err)
	}
}

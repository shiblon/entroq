package eqsvcgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestModifyAuthzCoversEveryOp is a completeness guard: every task and doc
// operation a Modify can carry MUST contribute an authorization requirement so
// that a future op type added without a matching requirement fails this test
// rather than silently slipping past the queue/namespace access boundary.
//
// Each op below uses a distinct queue/namespace so a missing requirement shows
// up as an absent map entry, not as one op masking another.
func TestModifyAuthzCoversEveryOp(t *testing.T) {
	s := &QSvc{} // no authorizer: modifyAuthz still builds the request it would authorize.

	req := &pb.ModifyRequest{
		Inserts: []*pb.TaskData{{Queue: "q-ins"}},
		Changes: []*pb.TaskChange{{
			OldId:   &pb.TaskID{Queue: "q-chg"},
			NewData: &pb.TaskData{Queue: "q-chg"},
		}},
		Deletes: []*pb.TaskID{{Queue: "q-del"}},
		Depends: []*pb.TaskID{{Queue: "q-dep"}},

		DocInserts: []*pb.DocData{{Namespace: "ns-ins"}},
		DocChanges: []*pb.DocChange{{
			OldId:   &pb.DocID{Namespace: "ns-chg"},
			NewData: &pb.DocData{Namespace: "ns-chg"},
		}},
		DocDeletes: []*pb.DocID{{Namespace: "ns-del"}},
		DocDepends: []*pb.DocID{{Namespace: "ns-dep"}},
	}

	authReq, err := s.modifyAuthz(context.Background(), req)
	if err != nil {
		t.Fatalf("modifyAuthz returned an unexpected error: %v", err)
	}

	wantQueues := map[string]authz.Action{
		"q-ins": authz.Insert,
		"q-chg": authz.Change,
		"q-del": authz.Delete,
		"q-dep": authz.Read,
	}
	gotQueues := map[string]authz.Action{}
	for _, q := range authReq.Queues {
		if len(q.Actions) != 1 {
			t.Errorf("queue %q: got %d actions, want exactly 1", q.Exact, len(q.Actions))
			continue
		}
		gotQueues[q.Exact] = q.Actions[0]
	}
	for name, want := range wantQueues {
		if got := gotQueues[name]; got != want {
			t.Errorf("queue %q: got action %q, want %q", name, got, want)
		}
	}
	if len(gotQueues) != len(wantQueues) {
		t.Errorf("queue requirements: got %d (%v), want %d (%v)", len(gotQueues), gotQueues, len(wantQueues), wantQueues)
	}

	wantNamespaces := map[string]authz.Action{
		"ns-ins": authz.Insert,
		"ns-chg": authz.Change,
		"ns-del": authz.Delete,
		"ns-dep": authz.Read,
	}
	gotNamespaces := map[string]authz.Action{}
	for _, n := range authReq.Namespaces {
		if len(n.Actions) != 1 {
			t.Errorf("namespace %q: got %d actions, want exactly 1", n.Exact, len(n.Actions))
			continue
		}
		gotNamespaces[n.Exact] = n.Actions[0]
	}
	for name, want := range wantNamespaces {
		if got := gotNamespaces[name]; got != want {
			t.Errorf("namespace %q: got action %q, want %q", name, got, want)
		}
	}
	if len(gotNamespaces) != len(wantNamespaces) {
		t.Errorf("namespace requirements: got %d (%v), want %d (%v)", len(gotNamespaces), gotNamespaces, len(wantNamespaces), wantNamespaces)
	}
}

// TestModifyAuthzFailsClosedOnEmptyTarget verifies that an operation naming no
// queue (or no namespace) is rejected rather than producing an authz request
// with a hole in it. An empty target must fail closed: authorization cannot be
// checked against a queue/namespace the caller never named.
func TestModifyAuthzFailsClosedOnEmptyTarget(t *testing.T) {
	s := &QSvc{}

	cases := map[string]*pb.ModifyRequest{
		"empty insert queue":  {Inserts: []*pb.TaskData{{Queue: ""}}},
		"empty delete queue":  {Deletes: []*pb.TaskID{{Queue: ""}}},
		"empty depend queue":  {Depends: []*pb.TaskID{{Queue: ""}}},
		"empty change queue":  {Changes: []*pb.TaskChange{{OldId: &pb.TaskID{Queue: ""}, NewData: &pb.TaskData{Queue: ""}}}},
		"empty doc insert ns": {DocInserts: []*pb.DocData{{Namespace: ""}}},
		"empty doc delete ns": {DocDeletes: []*pb.DocID{{Namespace: ""}}},
		"empty doc depend ns": {DocDepends: []*pb.DocID{{Namespace: ""}}},
		"empty doc change ns": {DocChanges: []*pb.DocChange{{OldId: &pb.DocID{Namespace: ""}, NewData: &pb.DocData{Namespace: ""}}}},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.modifyAuthz(context.Background(), req); err == nil {
				t.Errorf("modifyAuthz(%s): got nil error, want fail-closed rejection", name)
			}
		})
	}
}

// TestModifyAuthzMoveRequiresDeleteAndInsert verifies that a task change whose
// destination queue differs from its source is authorized as a move: Delete on
// the source, Insert on the destination. Only tasks move; docs are authorized in
// place (see TestModifyAuthzEmptyDestIsNoMove and TestModifyRejectsDocNamespaceChange).
func TestModifyAuthzMoveRequiresDeleteAndInsert(t *testing.T) {
	s := &QSvc{}
	// Only tasks move (between queues). Docs cannot move namespaces, so a doc
	// change is authorized in place (covered by TestModifyAuthzEmptyDestIsNoMove)
	// and a cross-namespace one is rejected (TestModifyRejectsDocNamespaceChange).
	req := &pb.ModifyRequest{
		Changes: []*pb.TaskChange{{
			OldId:   &pb.TaskID{Queue: "q-from"},
			NewData: &pb.TaskData{Queue: "q-to"},
		}},
	}
	authReq, err := s.modifyAuthz(context.Background(), req)
	if err != nil {
		t.Fatalf("modifyAuthz returned an unexpected error: %v", err)
	}

	wantQueues := map[string]authz.Action{"q-from": authz.Delete, "q-to": authz.Insert}
	gotQueues := map[string]authz.Action{}
	for _, q := range authReq.Queues {
		gotQueues[q.Exact] = q.Actions[0]
	}
	for name, want := range wantQueues {
		if got := gotQueues[name]; got != want {
			t.Errorf("move queue %q: got action %q, want %q", name, got, want)
		}
	}
}

// TestModifyAuthzEmptyDestIsNoMove verifies that a change with a source but an
// empty destination is treated as "no move" (Change on the source), not as a
// move into an empty target. This is the path today's doc client actually takes:
// it sets OldId.Namespace but leaves NewData.Namespace empty, so an empty
// destination must NOT fail closed.
func TestModifyAuthzEmptyDestIsNoMove(t *testing.T) {
	s := &QSvc{}
	req := &pb.ModifyRequest{
		Changes: []*pb.TaskChange{{
			OldId:   &pb.TaskID{Queue: "q"},
			NewData: &pb.TaskData{Queue: ""},
		}},
		DocChanges: []*pb.DocChange{{
			OldId:   &pb.DocID{Namespace: "ns"},
			NewData: &pb.DocData{Namespace: ""},
		}},
	}
	authReq, err := s.modifyAuthz(context.Background(), req)
	if err != nil {
		t.Fatalf("empty destination must be treated as no-move, got error: %v", err)
	}
	if len(authReq.Queues) != 1 || authReq.Queues[0].Exact != "q" || authReq.Queues[0].Actions[0] != authz.Change {
		t.Errorf("queue change: got %+v, want a single Change on %q", authReq.Queues, "q")
	}
	if len(authReq.Namespaces) != 1 || authReq.Namespaces[0].Exact != "ns" || authReq.Namespaces[0].Actions[0] != authz.Change {
		t.Errorf("namespace change: got %+v, want a single Change on %q", authReq.Namespaces, "ns")
	}
}

// TestModifyRejectsDocNamespaceChange verifies that a doc change naming a
// destination namespace different from its source is rejected with
// InvalidArgument rather than silently applied in the source namespace. Docs do
// not move between namespaces; this is the only path that can express such a
// change (the Go fluent API cannot). The rejection lives in Modify's conversion,
// so it holds even with no authorizer configured.
func TestModifyRejectsDocNamespaceChange(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	req := &pb.ModifyRequest{
		DocChanges: []*pb.DocChange{{
			OldId:   &pb.DocID{Namespace: "ns-a", Id: "d1"},
			NewData: &pb.DocData{Namespace: "ns-b"},
		}},
	}
	if _, err := svc.Modify(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cross-namespace doc change: got err %v (code %v), want InvalidArgument", err, status.Code(err))
	}

	// A same-namespace change (empty destination = unspecified) is NOT rejected
	// for the namespace reason; it fails only because the doc does not exist.
	req.DocChanges[0].NewData.Namespace = ""
	if _, err := svc.Modify(ctx, req); status.Code(err) == codes.InvalidArgument {
		t.Errorf("same-namespace doc change should not be an InvalidArgument, got %v", err)
	}
}

// stubAuthorizer is a test authz.Authorizer returning a fixed decision, so a
// test can drive Modify's enforcement seam without a live OPA. It records
// whether it was consulted, distinguishing "allowed" from "never asked".
type stubAuthorizer struct {
	err    error
	called bool
	req    *authz.Request
}

func (s *stubAuthorizer) Authorize(_ context.Context, req *authz.Request) error {
	s.called = true
	s.req = req
	return s.err
}

func (s *stubAuthorizer) Close() error { return nil }

type stubAuthenticator struct {
	principal *authn.VerifiedPrincipal
	err       error
	called    bool
	creds     *authn.Credentials
}

func (s *stubAuthenticator) Authenticate(_ context.Context, creds *authn.Credentials) (*authn.VerifiedPrincipal, error) {
	s.called = true
	if creds != nil {
		copy := *creds
		s.creds = &copy
	}
	return s.principal, s.err
}

func (s *stubAuthenticator) Close() error { return nil }

func allowingAuthenticator() *stubAuthenticator {
	return &stubAuthenticator{principal: &authn.VerifiedPrincipal{Subject: "service-a"}}
}

// TestModifyEnforcesAuthorizerDenial closes the loop that the modifyAuthz unit
// tests leave open: those prove the right authorization request is BUILT, but
// not that a denial actually blocks the modification. Here a configured
// authorizer denies, and Modify must surface PermissionDenied and never touch
// the backend. This guards the build -> authorize -> reject wiring end to end.
func TestModifyEnforcesAuthorizerDenial(t *testing.T) {
	ctx := context.Background()
	denied := &authz.AuthzError{
		Failed: []*authz.Queue{{Exact: "q", Actions: []authz.Action{authz.Insert}}},
	}
	az := &stubAuthorizer{err: denied}
	an := allowingAuthenticator()
	svc, err := New(ctx, eqmem.Opener(), WithAuthenticator(an), WithAuthorizer(az))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	req := &pb.ModifyRequest{Inserts: []*pb.TaskData{{Queue: "q"}}}
	if _, err := svc.Modify(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("denied modify: got err %v (code %v), want PermissionDenied", err, status.Code(err))
	}
	if !az.called {
		t.Error("authorizer was never consulted; denial did not come from authz")
	}
	if !an.called || az.req.Principal == nil || az.req.Principal.Subject != "service-a" {
		t.Fatalf("verified principal was not passed to authorizer: authn=%v request=%#v", an.called, az.req)
	}
}

// TestModifyAllowsWhenAuthorized is the positive counterpart: with an
// authorizer that allows, the same insert proceeds to the backend and succeeds.
// Together with the denial test this pins the authorizer's decision -- not some
// unrelated validation -- as what gates the modification.
func TestModifyAllowsWhenAuthorized(t *testing.T) {
	ctx := context.Background()
	az := &stubAuthorizer{err: nil}
	svc, err := New(ctx, eqmem.Opener(), WithAuthenticator(allowingAuthenticator()), WithAuthorizer(az))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	req := &pb.ModifyRequest{Inserts: []*pb.TaskData{{Queue: "q"}}}
	resp, err := svc.Modify(ctx, req)
	if err != nil {
		t.Fatalf("allowed modify: unexpected error: %v", err)
	}
	if !az.called {
		t.Error("authorizer was never consulted")
	}
	if len(resp.Inserted) != 1 {
		t.Errorf("inserted tasks = %d, want 1", len(resp.Inserted))
	}
}

func TestAuthenticationFailureStopsBeforeAuthorization(t *testing.T) {
	ctx := context.Background()
	an := &stubAuthenticator{err: authn.InvalidError("bad token", errors.New("signature"))}
	az := new(stubAuthorizer)
	svc, err := New(ctx, eqmem.Opener(), WithAuthenticator(an), WithAuthorizer(az))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	_, err = svc.Modify(ctx, &pb.ModifyRequest{Inserts: []*pb.TaskData{{Queue: "q"}}})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("authentication error = %v (code %v), want Unauthenticated", err, status.Code(err))
	}
	if az.called {
		t.Fatal("authorizer was called after authentication failed")
	}
}

func TestNewRequiresAuthenticationAndAuthorizationTogether(t *testing.T) {
	ctx := context.Background()
	for _, option := range []Option{
		WithAuthenticator(allowingAuthenticator()),
		WithAuthorizer(new(stubAuthorizer)),
	} {
		svc, err := New(ctx, eqmem.Opener(), option)
		if err == nil {
			svc.Close()
			t.Fatal("New accepted an incomplete authentication/authorization boundary")
		}
	}
}

func TestBearerTokenStopsAtAuthenticator(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret-token"))
	an := allowingAuthenticator()
	az := new(stubAuthorizer)
	svc, err := New(ctx, eqmem.Opener(), WithAuthenticator(an), WithAuthorizer(az))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	if _, err := svc.Modify(ctx, &pb.ModifyRequest{Inserts: []*pb.TaskData{{Queue: "q"}}}); err != nil {
		t.Fatalf("Modify: %v", err)
	}
	if an.creds == nil || an.creds.Scheme != "Bearer" || an.creds.Token != "secret-token" {
		t.Fatalf("authenticator credentials = %#v", an.creds)
	}
	encoded, err := json.Marshal(az.req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret-token")) || bytes.Contains(encoded, []byte("authz")) {
		t.Fatalf("authorization request leaked credentials: %s", encoded)
	}
}

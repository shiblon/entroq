package eqsvcgrpc

import (
	"context"
	"testing"

	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/authz"
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

// TestModifyAuthzMoveRequiresDeleteAndInsert verifies that a change whose
// destination differs from its source is authorized as a move: delete on the
// source, insert on the destination. This holds for queues and, for
// authz-readiness, for namespaces too, even though namespace moves are not yet
// implemented in any backend.
func TestModifyAuthzMoveRequiresDeleteAndInsert(t *testing.T) {
	s := &QSvc{}
	req := &pb.ModifyRequest{
		Changes: []*pb.TaskChange{{
			OldId:   &pb.TaskID{Queue: "q-from"},
			NewData: &pb.TaskData{Queue: "q-to"},
		}},
		DocChanges: []*pb.DocChange{{
			OldId:   &pb.DocID{Namespace: "ns-from"},
			NewData: &pb.DocData{Namespace: "ns-to"},
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

	wantNS := map[string]authz.Action{"ns-from": authz.Delete, "ns-to": authz.Insert}
	gotNS := map[string]authz.Action{}
	for _, n := range authReq.Namespaces {
		gotNS[n.Exact] = n.Actions[0]
	}
	for name, want := range wantNS {
		if got := gotNS[name]; got != want {
			t.Errorf("move namespace %q: got action %q, want %q", name, got, want)
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

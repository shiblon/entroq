package pbconv

import (
	"errors"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMSRoundTrip(t *testing.T) {
	// A millisecond-truncated time survives ToMS -> FromMS unchanged.
	want := time.Unix(0, 1_700_000_000_123*int64(time.Millisecond))
	if got := FromMS(ToMS(want)); !got.Equal(want) {
		t.Errorf("FromMS(ToMS(%v)) = %v, want %v", want, got, want)
	}
}

func TestJSONProtoRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"nil is no-value", nil},
		{"string", []byte(`"hi"`)},
		{"object", []byte(`{"a":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := JSONToProto(tc.raw)
			if err != nil {
				t.Fatalf("JSONToProto: %v", err)
			}
			got, err := ProtoToJSON(v)
			if err != nil {
				t.Fatalf("ProtoToJSON: %v", err)
			}
			if string(got) != string(tc.raw) {
				t.Errorf("round trip = %q, want %q", got, tc.raw)
			}
		})
	}
}

func TestModifyArgsFromProto(t *testing.T) {
	req := &pb.ModifyRequest{
		ClaimantId: "worker-1",
		Inserts:    []*pb.TaskData{{Queue: "out", Value: structpb.NewStringValue("hi")}},
		Deletes:    []*pb.TaskID{{Id: "abc", Version: 2, Queue: "in"}},
	}
	args, err := ModifyArgsFromProto(req, 2)
	if err != nil {
		t.Fatalf("ModifyArgsFromProto: %v", err)
	}
	// Assemble the modification the args describe and inspect it directly.
	m := entroq.NewModification("", args...)

	if m.Claimant != "worker-1" {
		t.Errorf("claimant = %q, want %q", m.Claimant, "worker-1")
	}
	if len(m.Inserts) != 1 || m.Inserts[0].Queue != "out" {
		t.Fatalf("inserts = %+v, want one into %q", m.Inserts, "out")
	}
	if got := string(m.Inserts[0].Value); got != `"hi"` {
		t.Errorf("insert value = %s, want %q", got, `"hi"`)
	}
	if len(m.Deletes) != 1 || m.Deletes[0].ID != "abc" || m.Deletes[0].Version != 2 || m.Deletes[0].Queue != "in" {
		t.Errorf("deletes = %+v, want abc:v2 in %q", m.Deletes, "in")
	}
}

func TestModifyArgsFromProtoRejectsNamespaceMove(t *testing.T) {
	req := &pb.ModifyRequest{
		DocChanges: []*pb.DocChange{{
			OldId:   DocIDToProto("ns-a", "d1", 0),
			NewData: &pb.DocData{Namespace: "ns-b"},
		}},
	}
	_, err := ModifyArgsFromProto(req, 2)
	var inv *InvalidRequestError
	if !errors.As(err, &inv) {
		t.Fatalf("cross-namespace doc change: got %v, want *InvalidRequestError", err)
	}
}

func TestDependencyErrorDetails(t *testing.T) {
	de := &entroq.DependencyError{
		Message:    "boom",
		Depends:    []*entroq.TaskID{{ID: "t1", Version: 1, Queue: "q"}},
		DocDeletes: []*entroq.DocID{{Namespace: "ns", ID: "d1", Version: 2}},
	}
	deps := DependencyErrorDetails(de)

	if len(deps) == 0 || deps[0].Type != pb.ActionType_DETAIL || deps[0].Msg != "boom" {
		t.Fatalf("first detail = %+v, want a DETAIL carrying %q", deps, "boom")
	}

	var gotTaskDepend, gotDocDelete bool
	for _, d := range deps[1:] {
		switch {
		case d.Type == pb.ActionType_DEPEND && d.Id.GetId() == "t1":
			gotTaskDepend = true
		case d.Type == pb.ActionType_DELETE && d.DocId.GetId() == "d1" && d.DocId.GetNamespace() == "ns":
			gotDocDelete = true
		}
	}
	if !gotTaskDepend {
		t.Errorf("missing DEPEND detail for task t1 in %+v", deps)
	}
	if !gotDocDelete {
		t.Errorf("missing DELETE detail for doc ns/d1 in %+v", deps)
	}
}

func modification(t *testing.T, req *pb.ModifyRequest) (*entroq.Modification, error) {
	t.Helper()
	args, err := ModifyArgsFromProto(req, 2)
	if err != nil {
		return nil, err
	}
	return entroq.NewModification("", args...), nil
}

func TestModifyArgsFromProtoRefusesMissingParts(t *testing.T) {
	old := &pb.TaskID{Id: "t", Version: 1, Queue: "q"}
	for name, req := range map[string]*pb.ModifyRequest{
		"change with no task":     {Changes: []*pb.TaskChange{{NewData: &pb.TaskData{}}}},
		"change with no data":     {Changes: []*pb.TaskChange{{OldId: old}}},
		"change of unknown mode":  {Changes: []*pb.TaskChange{{OldId: old, NewData: &pb.TaskData{}, Mode: pb.ChangeMode(99)}}},
		"doc change with no doc":  {DocChanges: []*pb.DocChange{{NewData: &pb.DocData{}}}},
		"doc change with no data": {DocChanges: []*pb.DocChange{{OldId: DocIDToProto("ns", "d", 0)}}},
		"doc delete naming none":  {DocDeletes: []*pb.DocID{{Namespace: "ns"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var inv *InvalidRequestError
			if _, err := ModifyArgsFromProto(req, 2); !errors.As(err, &inv) {
				t.Errorf("got %v, want *InvalidRequestError", err)
			}
		})
	}
}

func TestModifyArgsFromProtoLeases(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	mod, err := modification(t, &pb.ModifyRequest{
		Changes: []*pb.TaskChange{{
			OldId:   &pb.TaskID{Id: "t", Version: 3, Queue: "q"},
			NewData: &pb.TaskData{AtMs: ToMS(at)},
			Mode:    pb.ChangeMode_CHANGE_LEASE,
		}},
		DocChanges: []*pb.DocChange{{
			OldId:   DocSetIDToProto("ns", "k", 5),
			NewData: &pb.DocData{AtMs: ToMS(at)},
			Mode:    pb.ChangeMode_CHANGE_LEASE,
		}},
	})
	if err != nil {
		t.Fatalf("ModifyArgsFromProto: %v", err)
	}
	if len(mod.Changes) != 0 || len(mod.DocChanges) != 0 {
		t.Errorf("Leases became changes: %v", mod)
	}
	if len(mod.Arrives) != 1 || mod.Arrives[0].TaskID != (entroq.TaskID{ID: "t", Version: 3, Queue: "q"}) || !mod.Arrives[0].At.Equal(at) {
		t.Errorf("Task lease: got %+v", mod.Arrives)
	}
	if len(mod.DocArrives) != 1 || mod.DocArrives[0].DocID != *entroq.NewDocSetRef("ns", "k", 5) || !mod.DocArrives[0].At.Equal(at) {
		t.Errorf("Doc set lease: got %+v", mod.DocArrives)
	}
}

// TestModifyArgsFromProtoModes checks what each mode may carry, and that a
// request declaring protocol 1 may use none but the default.
func TestModifyArgsFromProtoModes(t *testing.T) {
	old := &pb.TaskID{Id: "t", Version: 1, Queue: "q"}
	lease := pb.ChangeMode_CHANGE_LEASE
	for name, tc := range map[string]struct {
		protocol int32
		req      *pb.ModifyRequest
	}{
		"task lease with a value":    {2, &pb.ModifyRequest{Changes: []*pb.TaskChange{{OldId: old, NewData: &pb.TaskData{Value: structpb.NewStringValue("x")}, Mode: lease}}}},
		"task lease moving it":       {2, &pb.ModifyRequest{Changes: []*pb.TaskChange{{OldId: old, NewData: &pb.TaskData{Queue: "elsewhere"}, Mode: lease}}}},
		"doc lease with content":     {2, &pb.ModifyRequest{DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), NewData: &pb.DocData{Content: structpb.NewStringValue("x")}, Mode: lease}}}},
		"doc lease of another key":   {2, &pb.ModifyRequest{DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), NewData: &pb.DocData{Key: "other"}, Mode: lease}}}},
		"doc claims reset":           {2, &pb.ModifyRequest{DocChanges: []*pb.DocChange{{OldId: DocIDToProto("ns", "d", 0), NewData: &pb.DocData{}, Mode: pb.ChangeMode_CHANGE_RESET_CLAIMS}}}},
		"task lease at protocol 1":   {1, &pb.ModifyRequest{Changes: []*pb.TaskChange{{OldId: old, NewData: &pb.TaskData{}, Mode: lease}}}},
		"claims reset at protocol 1": {1, &pb.ModifyRequest{Changes: []*pb.TaskChange{{OldId: old, NewData: &pb.TaskData{}, Mode: pb.ChangeMode_CHANGE_RESET_CLAIMS}}}},
		"doc lease at protocol 1":    {1, &pb.ModifyRequest{DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), NewData: &pb.DocData{}, Mode: lease}}}},
	} {
		t.Run(name, func(t *testing.T) {
			var inv *InvalidRequestError
			if _, err := ModifyArgsFromProto(tc.req, tc.protocol); !errors.As(err, &inv) {
				t.Errorf("got %v, want *InvalidRequestError", err)
			}
		})
	}
	if _, err := ModifyArgsFromProto(&pb.ModifyRequest{Changes: []*pb.TaskChange{{OldId: old, NewData: &pb.TaskData{Queue: "q"}}}}, 1); err != nil {
		t.Errorf("Default change at protocol 1: %v", err)
	}
}

func TestModifyArgsFromProtoNotYetSupported(t *testing.T) {
	for name, req := range map[string]*pb.ModifyRequest{
		"doc change by key": {DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), NewData: &pb.DocData{}}}},
		"doc delete by key": {DocDeletes: []*pb.DocID{DocSetIDToProto("ns", "k", 0)}},
		"doc lease by ID":   {DocChanges: []*pb.DocChange{{OldId: DocIDToProto("ns", "d", 0), NewData: &pb.DocData{}, Mode: pb.ChangeMode_CHANGE_LEASE}}},
	} {
		t.Run(name, func(t *testing.T) {
			var uns *UnsupportedRequestError
			if _, err := ModifyArgsFromProto(req, 2); !errors.As(err, &uns) {
				t.Errorf("got %v, want *UnsupportedRequestError", err)
			}
		})
	}
}

func TestDependencyErrorDetailsNameSets(t *testing.T) {
	deps := DependencyErrorDetails(&entroq.DependencyError{
		DocClaims:  []*entroq.DocID{entroq.NewDocSetRef("ns", "held", 2)},
		DocArrives: []*entroq.DocID{entroq.NewDocSetRef("ns", "stale", 4)},
	})
	found := make(map[string]pb.ActionType)
	for _, d := range deps[1:] {
		if d.GetDocId().GetId() != "" {
			t.Errorf("Set failure named by ID: %+v", d)
		}
		found[d.GetDocId().GetKey()] = d.Type
	}
	if found["held"] != pb.ActionType_CLAIM || found["stale"] != pb.ActionType_CHANGE {
		t.Errorf("Set failures: got %v", found)
	}
}

// TestModifyArgsFromProtoClaims checks that a change resets the task's claim
// count only in the mode that asks for it.
func TestModifyArgsFromProtoClaims(t *testing.T) {
	mod, err := modification(t, &pb.ModifyRequest{
		Changes: []*pb.TaskChange{
			{OldId: &pb.TaskID{Id: "kept", Version: 1, Queue: "q"}, NewData: &pb.TaskData{Queue: "q"}},
			{OldId: &pb.TaskID{Id: "reset", Version: 1, Queue: "q"}, NewData: &pb.TaskData{Queue: "q"}, Mode: pb.ChangeMode_CHANGE_RESET_CLAIMS},
		},
	})
	if err != nil {
		t.Fatalf("ModifyArgsFromProto: %v", err)
	}
	for _, c := range mod.Changes {
		if want := c.ID == "reset"; mod.ResetsClaims(c.ID) != want {
			t.Errorf("Change %s: resets claims %v, want %v", c.ID, mod.ResetsClaims(c.ID), want)
		}
	}
}

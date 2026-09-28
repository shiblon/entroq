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
	args, err := ModifyArgsFromProto(req)
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
			OldId: DocIDToProto("ns-a", "d1", 0),
			Data:  &pb.DocChange_NewData{NewData: &pb.DocData{Namespace: "ns-b"}},
		}},
	}
	_, err := ModifyArgsFromProto(req)
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
	args, err := ModifyArgsFromProto(req)
	if err != nil {
		return nil, err
	}
	return entroq.NewModification("", args...), nil
}

func TestModifyArgsFromProtoRefusesMissingParts(t *testing.T) {
	old := &pb.TaskID{Id: "t", Version: 1, Queue: "q"}
	for name, req := range map[string]*pb.ModifyRequest{
		"change with no task":      {Changes: []*pb.TaskChange{{Data: &pb.TaskChange_NewData{NewData: &pb.TaskData{}}}}},
		"change with no data":      {Changes: []*pb.TaskChange{{OldId: old}}},
		"doc change with no doc":   {DocChanges: []*pb.DocChange{{Data: &pb.DocChange_NewData{NewData: &pb.DocData{}}}}},
		"doc change with no data":  {DocChanges: []*pb.DocChange{{OldId: DocIDToProto("ns", "d", 0)}}},
		"doc delete naming none":   {DocDeletes: []*pb.DocID{{Namespace: "ns"}}},
		"task lease with a value":  {Changes: []*pb.TaskChange{{OldId: old, Data: &pb.TaskChange_NewLease{NewLease: &pb.TaskData{Value: structpb.NewStringValue("x")}}}}},
		"task lease moving it":     {Changes: []*pb.TaskChange{{OldId: old, Data: &pb.TaskChange_NewLease{NewLease: &pb.TaskData{Queue: "elsewhere"}}}}},
		"doc lease with content":   {DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), Data: &pb.DocChange_NewLease{NewLease: &pb.DocData{Content: structpb.NewStringValue("x")}}}}},
		"doc lease of another key": {DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), Data: &pb.DocChange_NewLease{NewLease: &pb.DocData{Key: "other"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			var inv *InvalidRequestError
			if _, err := ModifyArgsFromProto(req); !errors.As(err, &inv) {
				t.Errorf("got %v, want *InvalidRequestError", err)
			}
		})
	}
}

func TestModifyArgsFromProtoLeases(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	mod, err := modification(t, &pb.ModifyRequest{
		Changes: []*pb.TaskChange{{
			OldId: &pb.TaskID{Id: "t", Version: 3, Queue: "q"},
			Data:  &pb.TaskChange_NewLease{NewLease: &pb.TaskData{AtMs: ToMS(at)}},
		}},
		DocChanges: []*pb.DocChange{{
			OldId: DocSetIDToProto("ns", "k", 5),
			Data:  &pb.DocChange_NewLease{NewLease: &pb.DocData{AtMs: ToMS(at)}},
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

func TestModifyArgsFromProtoNotYetSupported(t *testing.T) {
	for name, req := range map[string]*pb.ModifyRequest{
		"doc change by key": {DocChanges: []*pb.DocChange{{OldId: DocSetIDToProto("ns", "k", 0), Data: &pb.DocChange_NewData{NewData: &pb.DocData{}}}}},
		"doc delete by key": {DocDeletes: []*pb.DocID{DocSetIDToProto("ns", "k", 0)}},
		"doc lease by ID":   {DocChanges: []*pb.DocChange{{OldId: DocIDToProto("ns", "d", 0), Data: &pb.DocChange_NewLease{NewLease: &pb.DocData{}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			var uns *UnsupportedRequestError
			if _, err := ModifyArgsFromProto(req); !errors.As(err, &uns) {
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

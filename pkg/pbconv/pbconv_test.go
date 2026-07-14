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
			OldId:   &pb.DocID{Namespace: "ns-a", Id: "d1"},
			NewData: &pb.DocData{Namespace: "ns-b"},
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

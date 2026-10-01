package eqsvcgrpc

import (
	"context"
	"strings"
	"testing"

	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// withUnknown adds a field numbered num, which no message here declares, to m.
func withUnknown(m proto.Message, num protowire.Number) {
	r := m.ProtoReflect()
	raw := protowire.AppendTag(append([]byte(nil), r.GetUnknown()...), num, protowire.VarintType)
	r.SetUnknown(protowire.AppendVarint(raw, 1))
}

func TestCheckKnown(t *testing.T) {
	clean := &pb.ModifyRequest{
		ClaimantId: "me",
		Inserts:    []*pb.TaskData{{Queue: "q"}},
		Changes:    []*pb.TaskChange{{OldId: &pb.TaskID{Id: "t", Queue: "q"}, NewData: &pb.TaskData{Queue: "q"}}},
	}
	if err := checkKnown(clean, 2); err != nil {
		t.Errorf("Request with only known fields: %v", err)
	}

	req := proto.Clone(clean).(*pb.ModifyRequest)
	withUnknown(req, 90)
	withUnknown(req.Inserts[0], 91)
	withUnknown(req.Changes[0].NewData, 92)
	err := checkKnown(req, 2)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Request with unknown fields: want InvalidArgument, got %v", err)
	}
	for _, want := range []string{"field 90", "inserts[0] field 91", "changes[0].new_data field 92", "protocol 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error %q: want it to name %q", err, want)
		}
	}
}

// TestServiceRefusesUnknownFields checks that the service refuses a request
// with an unknown field before acting on it, whatever its protocol.
func TestServiceRefusesUnknownFields(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()
	req := &pb.ModifyRequest{ClaimantId: "me", Inserts: []*pb.TaskData{{Queue: "q"}}}
	withUnknown(req.Inserts[0], 99)
	if _, err := svc.Modify(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Modify with an unknown field: want InvalidArgument, got %v", err)
	}
	resp, err := svc.Tasks(ctx, &pb.TasksRequest{Queue: "q"})
	if err != nil || len(resp.GetTasks()) != 0 {
		t.Errorf("After the refused modify: want nothing inserted, got %v, %v", resp, err)
	}
}

func BenchmarkCheckKnown(b *testing.B) {
	req := &pb.ModifyRequest{ClaimantId: "me"}
	for range 100 {
		req.Changes = append(req.Changes, &pb.TaskChange{
			OldId:   &pb.TaskID{Id: "t", Queue: "q", Version: 1},
			NewData: &pb.TaskData{Queue: "q", AtMs: 1, Attempt: 2, Err: "e"},
		})
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := checkKnown(req, 2); err != nil {
			b.Fatal(err)
		}
	}
}

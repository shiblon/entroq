package eqgrpc

import (
	"slices"
	"testing"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc/metadata"
)

func TestServedOf(t *testing.T) {
	for _, tc := range []struct {
		md   metadata.MD
		want []int32
	}{
		{metadata.Pairs("content-type", "application/grpc"), []int32{1}},
		{metadata.Pairs(version.ProtocolHeader, "2"), []int32{2}},
		{metadata.Pairs(version.ProtocolHeader, "1,2"), []int32{1, 2}},
		{metadata.Pairs(version.ProtocolHeader, "nonsense"), []int32{1}},
	} {
		if got := servedOf(tc.md); !slices.Equal(got, tc.want) {
			t.Errorf("servedOf(%v) = %v, want %v", tc.md, got, tc.want)
		}
	}
}

// TestChangeModes checks that a change asks for a claims reset exactly when
// its modification says so.
func TestChangeModes(t *testing.T) {
	task := &entroq.Task{ID: "t", Queue: "q", Version: 1}
	for _, tc := range []struct {
		name string
		args []entroq.ChangeArg
		want pb.ChangeMode
	}{
		{"default", nil, pb.ChangeMode_CHANGE_DEFAULT},
		{"resetting claims", []entroq.ChangeArg{entroq.ResettingClaims()}, pb.ChangeMode_CHANGE_RESET_CLAIMS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := new(backend).modifyRequest(entroq.NewModification("me", task.Change(tc.args...)))
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if pc := req.GetChanges()[0]; pc.GetMode() != tc.want || pc.GetNewData() == nil {
				t.Errorf("Change: want mode %v with data, got %v", tc.want, pc)
			}
		})
	}
}

// TestArrivalsAreLeases checks that arrivals travel as lease-only changes in
// Modify, alongside the modification's other operations.
func TestArrivalsAreLeases(t *testing.T) {
	task := &entroq.Task{ID: "t", Queue: "q", Version: 1}
	set := &entroq.DocSet{Namespace: "ns", Key: "k", Version: 4}
	req, err := new(backend).modifyRequest(entroq.NewModification("me",
		entroq.Arriving(entroq.ReadyNow().Tasks(task).Docs(set)),
		entroq.InsertingInto("out"),
	))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(req.GetInserts()) != 1 || len(req.GetChanges()) != 1 || len(req.GetDocChanges()) != 1 {
		t.Fatalf("Request: want the insert and both leases, got %v", req)
	}
	if c := req.GetChanges()[0]; c.GetMode() != pb.ChangeMode_CHANGE_LEASE || c.GetOldId().GetId() != "t" {
		t.Errorf("Task arrival: want a lease of t, got %v", c)
	}
	if c := req.GetDocChanges()[0]; c.GetMode() != pb.ChangeMode_CHANGE_LEASE || c.GetOldId().GetKey() != "k" {
		t.Errorf("Set arrival: want a lease of set k, got %v", c)
	}
}

package eqgrpc

import (
	"testing"
	"time"

	pb "github.com/shiblon/entroq/api"
)

// TestClaimedSets checks that a ClaimDocs response's members go to their
// sets, in the order the sets came, and that a set claimed without its
// members comes back with none.
func TestClaimedSets(t *testing.T) {
	at := time.UnixMilli(time.Now().Add(time.Minute).UnixMilli())
	resp := &pb.ClaimDocsResponse{
		Sets: []*pb.Doc{
			{Namespace: "ns", Key: "b", Version: 3, Claimant: "me", AtMs: at.UnixMilli(), Len: 1},
			{Namespace: "ns", Key: "a", Version: 7, Claimant: "me", AtMs: at.UnixMilli(), Len: 2},
			{Namespace: "ns", Key: "omitted", Version: 1, Claimant: "me", AtMs: at.UnixMilli(), Len: 5},
		},
		Docs: []*pb.Doc{
			{Namespace: "ns", Id: "a1", Key: "a", Version: 7, Claimant: "me", AtMs: at.UnixMilli()},
			{Namespace: "ns", Id: "b1", Key: "b", Version: 3, Claimant: "me", AtMs: at.UnixMilli()},
			{Namespace: "ns", Id: "a2", Key: "a", Version: 7, Claimant: "me", AtMs: at.UnixMilli()},
		},
	}
	sets, err := claimedSets(resp)
	if err != nil {
		t.Fatalf("claimedSets: %v", err)
	}
	if len(sets) != 3 || sets[0].Key != "b" || sets[1].Key != "a" || sets[2].Key != "omitted" {
		t.Fatalf("Sets: want b, a, omitted in that order, got %+v", sets)
	}
	if len(sets[0].Docs) != 1 || len(sets[1].Docs) != 2 || len(sets[2].Docs) != 0 {
		t.Errorf("Members: want 1, 2, and none, got %d, %d, %d", len(sets[0].Docs), len(sets[1].Docs), len(sets[2].Docs))
	}
	if g := sets[1]; g.Version != 7 || g.Claimant != "me" || !g.At.Equal(at) || g.NumDocs != 2 {
		t.Errorf("Set from the response: got %+v", g)
	}
	if sets[2].NumDocs != 5 {
		t.Errorf("Set claimed without members: want its count, 5, got %d", sets[2].NumDocs)
	}
}

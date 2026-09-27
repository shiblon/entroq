package eqgrpc

import (
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
)

// TestClaimedGroupFromOlderServer checks the set a client builds from a
// ClaimDocs response without one, as servers before 1.13 send: its version and
// claim come from the members, which carry them.
func TestClaimedGroupFromOlderServer(t *testing.T) {
	cq := entroq.ClaimKey("ns", "k")
	at := time.UnixMilli(time.Now().Add(time.Minute).UnixMilli())
	resp := &pb.ClaimDocsResponse{Docs: []*pb.Doc{
		{Namespace: "ns", Id: "a", Key: "k", Version: 7, Claimant: "me", AtMs: at.UnixMilli()},
		{Namespace: "ns", Id: "b", Key: "k", Version: 7, Claimant: "me", AtMs: at.UnixMilli()},
	}}
	g := claimedSet(cq, resp)
	if g.Namespace != "ns" || g.Key != "k" || g.Version != 7 || g.Claimant != "me" || !g.At.Equal(at) || len(g.Docs) != 2 {
		t.Errorf("Set rebuilt from members: got %+v", g)
	}

	empty := claimedSet(cq, &pb.ClaimDocsResponse{})
	if empty.Namespace != "ns" || empty.Key != "k" || len(empty.Docs) != 0 {
		t.Errorf("Empty set from an older server: got %+v", empty)
	}
}

func TestClaimedGroupFromResponse(t *testing.T) {
	at := time.UnixMilli(time.Now().Add(time.Minute).UnixMilli())
	resp := &pb.ClaimDocsResponse{Sets: []*pb.Doc{{Namespace: "ns", Key: "k", Version: 3, Claimant: "me", AtMs: at.UnixMilli()}}}
	g := claimedSet(entroq.ClaimKey("ns", "k"), resp)
	if g.Version != 3 || g.Claimant != "me" || !g.At.Equal(at) || len(g.Docs) != 0 {
		t.Errorf("Set from the response: got %+v", g)
	}
}

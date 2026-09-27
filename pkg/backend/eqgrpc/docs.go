package eqgrpc

import (
	"context"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/pbconv"

	pb "github.com/shiblon/entroq/api"
)

// fromDocProto converts a proto Doc to an entroq.Doc.
// Docs returns a slice of docs in a namespace.
func (b *backend) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	resp, err := b.client().Docs(ctx, &pb.DocsRequest{
		Query: &pb.DocQuery{
			Namespace:  rq.Namespace,
			Ids:        rq.IDs,
			KeyExact:   rq.KeyExact,
			KeyStart:   rq.KeyStart,
			KeyEnd:     rq.KeyEnd,
			Limit:      int32(rq.Limit),
			OmitValues: rq.OmitValues,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("grpc docs: %w", unpackGRPCError(err))
	}
	docs := make([]*entroq.Doc, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		docs = append(docs, pbconv.MustDocFromProto(d))
	}
	return docs, nil
}

// ClaimDocs claims the doc group sharing a primary key in the namespace and
// returns it, with its members. Returns a DependencyError while someone else
// holds it. A server at protocol 2 is asked for the set by key, and answers
// with it; an older one is asked by namespace and key, claimed nothing for a
// group with no docs, and sends no group; see claimedGroup.
//
// TODO: allow multiple claim sets at once when backends support it.
func (b *backend) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) (*entroq.DocGroup, error) {
	p, err := b.serverProtocol(ctx)
	if err != nil {
		return nil, fmt.Errorf("grpc claim docs: %w", err)
	}
	claim := &pb.DocClaim{
		Claimant:   cq.Claimant,
		DurationMs: int64(cq.Duration / time.Millisecond),
	}
	if p >= 2 {
		claim.Sets = []*pb.DocID{pbconv.DocSetIDToProto(cq.Namespace, cq.Key, 0)}
	} else {
		claim.Namespace, claim.Key = cq.Namespace, cq.Key
	}
	resp, err := b.client().ClaimDocs(ctx, &pb.ClaimDocsRequest{ClaimQuery: claim})
	if err != nil {
		return nil, fmt.Errorf("grpc claim docs: %w", unpackGRPCError(err))
	}
	return claimedGroup(cq, resp), nil
}

// claimedGroup is the doc group a ClaimDocs response describes. A server at
// protocol 1 sends no sets, so the group is rebuilt from the members, which
// carry its version and claim; with no members there is nothing to rebuild
// from.
func claimedGroup(cq *entroq.DocClaim, resp *pb.ClaimDocsResponse) *entroq.DocGroup {
	docs := make([]*entroq.Doc, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		docs = append(docs, pbconv.MustDocFromProto(d))
	}
	if sets := resp.GetSets(); len(sets) > 0 {
		return pbconv.DocGroupFromProto(sets[0], docs)
	}
	g := &entroq.DocGroup{Namespace: cq.Namespace, Key: cq.Key, NumDocs: len(docs), Docs: docs}
	if len(docs) > 0 {
		g.Version, g.Claimant, g.At = docs[0].Version, docs[0].Claimant, docs[0].At
	}
	return g
}

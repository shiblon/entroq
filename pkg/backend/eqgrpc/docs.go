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
	resp, err := pb.NewEntroQClient(b.conn).Docs(ctx, &pb.DocsRequest{
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
// holds it. A server older than 1.13 claimed nothing for a group with no docs,
// and sends no group; see claimedGroup.
func (b *backend) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) (*entroq.DocGroup, error) {
	resp, err := pb.NewEntroQClient(b.conn).ClaimDocs(ctx, &pb.ClaimDocsRequest{
		ClaimQuery: &pb.DocClaim{
			Namespace:  cq.Namespace,
			Claimant:   cq.Claimant,
			Key:        cq.Key,
			DurationMs: int64(cq.Duration / time.Millisecond),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("grpc claim docs: %w", unpackGRPCError(err))
	}
	return claimedGroup(cq, resp), nil
}

// claimedGroup is the doc group a ClaimDocs response describes. A server older
// than 1.13 sends no group, so it is rebuilt from the members, which carry the
// group's version and claim; with no members there is nothing to rebuild from.
func claimedGroup(cq *entroq.DocClaim, resp *pb.ClaimDocsResponse) *entroq.DocGroup {
	docs := make([]*entroq.Doc, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		docs = append(docs, pbconv.MustDocFromProto(d))
	}
	if resp.Group != nil {
		return pbconv.DocGroupFromProto(resp.Group, docs)
	}
	g := &entroq.DocGroup{Namespace: cq.Namespace, Key: cq.Key, Docs: docs}
	if len(docs) > 0 {
		g.Version, g.Claimant, g.At = docs[0].Version, docs[0].Claimant, docs[0].At
	}
	return g
}

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

// ClaimDocs claims every doc set cq names, all or none, and returns them in
// the order named, each with its members unless claimed OmitMembers. Returns a
// DependencyError while someone else holds any of them. Sets are named by key,
// and the server answers with each.
func (b *backend) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.DocSet, error) {
	claim := &pb.DocClaim{
		Claimant:   cq.Claimant,
		DurationMs: int64(cq.Duration / time.Millisecond),
	}
	if !cq.At.IsZero() {
		claim.AtMs = pbconv.ToMS(cq.At)
	}
	for _, s := range cq.Sets {
		claim.Sets = append(claim.Sets, &pb.DocSetClaim{
			Set:         pbconv.DocSetIDToProto(s.Namespace, s.Key, 0),
			OmitMembers: s.OmitMembers,
		})
	}
	resp, err := b.client().ClaimDocs(ctx, &pb.ClaimDocsRequest{ClaimQuery: claim})
	if err != nil {
		return nil, fmt.Errorf("grpc claim docs: %w", unpackGRPCError(err))
	}
	return claimedSets(resp)
}

// claimedSets is the doc sets a ClaimDocs response describes, in order, each
// with the members that came back for it.
func claimedSets(resp *pb.ClaimDocsResponse) ([]*entroq.DocSet, error) {
	members := make(map[[2]string][]*entroq.Doc)
	for _, d := range resp.GetDocs() {
		doc := pbconv.MustDocFromProto(d)
		k := [2]string{doc.Namespace, doc.Key}
		members[k] = append(members[k], doc)
	}
	sets := make([]*entroq.DocSet, 0, len(resp.GetSets()))
	for _, g := range resp.GetSets() {
		sets = append(sets, pbconv.DocSetFromProto(g, members[[2]string{g.GetNamespace(), g.GetKey()}]))
	}
	return sets, nil
}

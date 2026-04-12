package eqgrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shiblon/entroq"

	pb "github.com/shiblon/entroq/api"
)

// fromDocProto converts a proto Doc to an entroq.Doc.
func fromDocProto(d *pb.Doc) *entroq.Doc {
	return &entroq.Doc{
		Namespace:    d.Namespace,
		ID:           d.Id,
		Version:      d.Version,
		Claimant:     d.Claimant,
		At:           fromMS(d.AtMs),
		ExpiresAt:    fromMS(d.ExpiresAtMs),
		KeyPrimary:   d.KeyPrimary,
		KeySecondary: d.KeySecondary,
		Content:      json.RawMessage(d.Content),
		Created:      fromMS(d.CreatedMs),
		Modified:     fromMS(d.ModifiedMs),
	}
}

// Docs returns a slice of docs in a namespace.
func (b *backend) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	resp, err := pb.NewEntroQClient(b.conn).Docs(ctx, &pb.DocsRequest{
		Query: &pb.DocQuery{
			Namespace:  rq.Namespace,
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
		docs = append(docs, fromDocProto(d))
	}
	return docs, nil
}

// ClaimDocs attempts an all-or-nothing claim of docs matching the query.
// Returns a DependencyError if any docs are missing or already claimed.
func (b *backend) ClaimDocs(ctx context.Context, cq *entroq.DocClaimQuery) ([]*entroq.Doc, error) {
	resp, err := pb.NewEntroQClient(b.conn).ClaimDocs(ctx, &pb.ClaimDocsRequest{
		ClaimQuery: &pb.DocClaimQuery{
			Namespace:  cq.Namespace,
			Claimant:   cq.Claimant,
			Ids:        cq.IDs,
			KeyStart:   cq.KeyStart,
			KeyEnd:     cq.KeyEnd,
			DurationMs: int64(cq.Duration / time.Millisecond),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("grpc claim docs: %w", unpackGRPCError(err))
	}
	docs := make([]*entroq.Doc, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		docs = append(docs, fromDocProto(d))
	}
	return docs, nil
}

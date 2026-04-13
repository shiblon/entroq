package eqgrpc

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"

	pb "github.com/shiblon/entroq/api"
)

// fromDocProto converts a proto Doc to an entroq.Doc.
func fromDocProto(d *pb.Doc) *entroq.Doc {
	content, err := protoToJSON(d.Content)
	if err != nil {
		log.Printf("ERROR: doc content conversion: %v", err)
	}
	return &entroq.Doc{
		Namespace:    d.Namespace,
		ID:           d.Id,
		Version:      d.Version,
		Claimant:     d.Claimant,
		At:           fromMS(d.AtMs),
		ExpiresAt:    fromMS(d.ExpiresAtMs),
		Key:          d.Key,
		SecondaryKey: d.SecondaryKey,
		Content:      content,
		Created:      fromMS(d.CreatedMs),
		Modified:     fromMS(d.ModifiedMs),
	}
}

// Docs returns a slice of docs in a namespace.
func (b *backend) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	resp, err := pb.NewEntroQClient(b.conn).Docs(ctx, &pb.DocsRequest{
		Query: &pb.DocQuery{
			Namespace:  rq.Namespace,
			Ids:        rq.IDs,
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

// ClaimDocs attempts an all-or-nothing claim of all docs with the given
// primary key in the namespace. Returns a DependencyError if any are claimed.
func (b *backend) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
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
	docs := make([]*entroq.Doc, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		docs = append(docs, fromDocProto(d))
	}
	return docs, nil
}

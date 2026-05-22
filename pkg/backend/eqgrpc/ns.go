package eqgrpc

import (
	"context"
	"fmt"

	"github.com/shiblon/entroq"

	pb "github.com/shiblon/entroq/api"
)

func (b *backend) NamespaceStats(ctx context.Context, qq *entroq.MatchQuery) (map[string]*entroq.NamespaceStat, error) {
	resp, err := pb.NewEntroQClient(b.conn).NamespaceStats(ctx, &pb.NamespacesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("grpc namespace stats: %w", unpackGRPCError(err))
	}
	ns := make(map[string]*entroq.NamespaceStat)
	for _, s := range resp.Namespaces {
		ns[s.Name] = &entroq.NamespaceStat{
			Name:    s.Name,
			Size:    int(s.NumDocs),
			Claimed: int(s.NumClaimed),
		}
	}
	return ns, nil
}

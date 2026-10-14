package eqmr

import (
	"context"
	"encoding/json"
	"io"
)

// objectStore stores opaque immutable objects for an IntermediateStore, which
// owns their encoding. put names the run an object belongs to, and sweep
// deletes everything held for a run.
type objectStore interface {
	put(ctx context.Context, run, objectID string, body io.Reader) (json.RawMessage, error)
	open(ctx context.Context, ref json.RawMessage) (io.ReadCloser, error)
	delete(ctx context.Context, ref json.RawMessage) error
	sweep(ctx context.Context, run string) error
}

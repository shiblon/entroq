package eqmr

import (
	"context"
	"encoding/json"
	"io"
)

// objectStore is the byte-oriented boundary beneath an intermediateStore.
// The caller owns the run encoding; an implementation only commits, opens,
// and deletes opaque immutable objects.
//
// put names the run an object belongs to, so a store can keep a run's objects
// together, and sweep deletes everything a store holds for a run.
type objectStore interface {
	put(ctx context.Context, run, objectID string, body io.Reader) (json.RawMessage, error)
	open(ctx context.Context, ref json.RawMessage) (io.ReadCloser, error)
	delete(ctx context.Context, ref json.RawMessage) error
	sweep(ctx context.Context, run string) error
}

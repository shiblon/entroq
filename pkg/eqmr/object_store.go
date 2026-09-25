package eqmr

import (
	"context"
	"encoding/json"
	"io"
)

// objectStore is the byte-oriented boundary beneath an intermediateStore.
// The caller owns the run encoding; an implementation only commits, opens,
// and deletes opaque immutable objects.
type objectStore interface {
	put(context.Context, string, io.Reader) (json.RawMessage, error)
	open(context.Context, json.RawMessage) (io.ReadCloser, error)
	delete(context.Context, json.RawMessage) error
}

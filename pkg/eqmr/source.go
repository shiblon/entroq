package eqmr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/shiblon/entroq/pkg/worker"
)

// Source reads a run's input. Setup lists its splits, one map task each, and a
// mapper opens the split it is given.
//
// A Source is configured in every process that maps for a run, and the run
// names it by Descriptor. Errors are retried, except a worker.MoveError, which
// fails the run at once with its reason.
type Source interface {
	// Descriptor identifies the source. See Descriptor.
	Descriptor() Descriptor

	// Splits lists the input's splits as opaque references, the same ones every
	// time, so a retried Setup recognizes what it staged.
	Splits(ctx context.Context) ([]json.RawMessage, error)

	// Open reads the records of one split.
	Open(ctx context.Context, split json.RawMessage) (Records, error)
}

// Records reads one split's records in order.
type Records interface {
	// Next returns the next record, or io.EOF after the last.
	Next(ctx context.Context) (*KV, error)
	Close() error
}

// sources is the registry of sources.
type sources = *registry[Source]

func newSources(ss ...Source) (sources, error) {
	return newRegistry("source", Source.Descriptor, ss...)
}

// splitDoc is the content of a split doc: the source's reference to one split.
type splitDoc struct {
	Split json.RawMessage `json:"split"`
}

const (
	inlineSourceName   = "inline"
	inlineSourceDriver = "entroq.eqmr.inline/1"
)

// inlineSource is input given to Setup directly. Each split's reference is its
// records, so the split doc holds the data.
type inlineSource struct {
	desc  Descriptor
	input []*KV
	n     int
}

func (c *Controller) inlineSource(input []*KV, n int) *inlineSource {
	return &inlineSource{
		desc:  Descriptor{Name: inlineSourceName, Driver: inlineSourceDriver, Identity: fitText(c.DocNS(), maxDescriptorFieldBytes)},
		input: input,
		n:     n,
	}
}

func (s *inlineSource) Descriptor() Descriptor { return s.desc }

// Splits divides the input into at most n contiguous splits whose sizes differ
// by at most one.
func (s *inlineSource) Splits(context.Context) ([]json.RawMessage, error) {
	var splits []json.RawMessage
	for _, split := range splitInput(s.input, s.n) {
		if len(split) == 0 {
			continue
		}
		raw, err := json.Marshal(split)
		if err != nil {
			return nil, fmt.Errorf("encode split: %w", err)
		}
		splits = append(splits, raw)
	}
	return splits, nil
}

func (s *inlineSource) Open(_ context.Context, split json.RawMessage) (Records, error) {
	var kvs []*KV
	if err := json.Unmarshal(split, &kvs); err != nil {
		return nil, worker.MoveErrorf("parse inline split: %v", err)
	}
	return &sliceRecords{kvs: kvs}, nil
}

// sliceRecords reads records from memory.
type sliceRecords struct {
	kvs []*KV
}

func (r *sliceRecords) Next(context.Context) (*KV, error) {
	if len(r.kvs) == 0 {
		return nil, io.EOF
	}
	kv := r.kvs[0]
	r.kvs = r.kvs[1:]
	return kv, nil
}

func (r *sliceRecords) Close() error { return nil }

// splitInput divides input into n contiguous groups whose sizes differ by at
// most one. Groups may be empty when len(input) < n.
func splitInput(input []*KV, n int) [][]*KV {
	splits := make([][]*KV, n)
	if len(input) == 0 {
		return splits
	}
	per, extra := len(input)/n, len(input)%n
	start := 0
	for i := range n {
		size := per
		if i < extra {
			size++
		}
		splits[i] = input[start : start+size]
		start += size
	}
	return splits
}

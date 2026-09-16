package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shiblon/entroq"
)

const (
	documentIntermediateStoreName = "documents"
	intermediateRunPrefix         = "run/"
	documentRunChunkBytes         = 64 << 10
)

type documentIntermediateStore struct {
	client    *entroq.EntroQ
	namespace string
}

func (c *Controller) intermediateStore() intermediateStore {
	return &documentIntermediateStore{client: c.client, namespace: c.DocNS()}
}

type documentRunRef struct {
	Namespace string                `json:"namespace"`
	Chunks    []documentRunChunkRef `json:"chunks"`
	Records   int                   `json:"records"`
}

type documentRunChunkRef struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`
}

func (s *documentIntermediateStore) name() string { return documentIntermediateStoreName }

func (s *documentIntermediateStore) put(ctx context.Context, records []intermediateRecord) (json.RawMessage, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("put document run: no records")
	}
	nonce := entroq.GenHex16()
	stamp := time.Now().UTC().Format("20060102-150405.000000000")
	ref := documentRunRef{Namespace: s.namespace, Records: len(records)}
	for i, chunk := range cutIntermediateRecords(records, documentRunChunkBytes) {
		id := fmt.Sprintf("run-%s-%06d", nonce, i)
		key := fmt.Sprintf("%s%s-%s/%06d", intermediateRunPrefix, stamp, nonce, i)
		resp, err := s.client.Modify(ctx, entroq.PuttingDocInto(s.namespace,
			entroq.WithIDKeys(id, key, ""),
			entroq.WithContent(chunk),
		))
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("put document run chunk %d: %w", i, err),
				s.deleteRef(ctx, ref),
			)
		}
		if len(resp.InsertedDocs) != 1 {
			return nil, errors.Join(
				fmt.Errorf("put document run chunk %d: got %d inserted docs, want 1", i, len(resp.InsertedDocs)),
				s.deleteRef(ctx, ref),
			)
		}
		doc := resp.InsertedDocs[0]
		ref.Chunks = append(ref.Chunks, documentRunChunkRef{ID: doc.ID, Version: doc.Version})
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("marshal document run reference: %w", err),
			s.deleteRef(ctx, ref),
		)
	}
	return raw, nil
}

func (s *documentIntermediateStore) open(_ context.Context, raw json.RawMessage) (intermediateReader, error) {
	ref, err := decodeDocumentRunRef(raw)
	if err != nil {
		return nil, err
	}
	return &documentRunReader{store: s, ref: ref}, nil
}

func (s *documentIntermediateStore) delete(ctx context.Context, raw json.RawMessage) error {
	ref, err := decodeDocumentRunRef(raw)
	if err != nil {
		return err
	}
	return s.deleteRef(ctx, ref)
}

func (s *documentIntermediateStore) deleteRef(ctx context.Context, ref documentRunRef) error {
	var errs []error
	for _, chunk := range ref.Chunks {
		_, err := s.client.Modify(ctx, entroq.NewDocID(ref.Namespace, chunk.ID, chunk.Version).Delete())
		if err != nil {
			if de, ok := entroq.AsDependency(err); ok && de.HasMissingDocs() {
				continue
			}
			errs = append(errs, fmt.Errorf("delete document run chunk %q: %w", chunk.ID, err))
		}
	}
	return errors.Join(errs...)
}

func decodeDocumentRunRef(raw json.RawMessage) (documentRunRef, error) {
	var ref documentRunRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return ref, fmt.Errorf("parse document run reference: %w", err)
	}
	if ref.Namespace == "" || len(ref.Chunks) == 0 || ref.Records <= 0 {
		return ref, fmt.Errorf("invalid document run reference: namespace, chunks, and record count are required")
	}
	for i, chunk := range ref.Chunks {
		if chunk.ID == "" || chunk.Version < 0 {
			return ref, fmt.Errorf("invalid document run reference: chunk %d requires an id and non-negative version", i)
		}
	}
	return ref, nil
}

type documentRunReader struct {
	store       *documentIntermediateStore
	ref         documentRunRef
	chunkIndex  int
	records     []intermediateRecord
	recordIndex int
	read        int
	closed      bool
}

func (r *documentRunReader) Next(ctx context.Context) (intermediateRecord, error) {
	if r.closed {
		return intermediateRecord{}, io.EOF
	}
	select {
	case <-ctx.Done():
		return intermediateRecord{}, ctx.Err()
	default:
	}
	for r.recordIndex >= len(r.records) {
		if r.chunkIndex >= len(r.ref.Chunks) {
			if r.read != r.ref.Records {
				return intermediateRecord{}, fmt.Errorf("document run ended after %d records, pointer requires %d", r.read, r.ref.Records)
			}
			return intermediateRecord{}, io.EOF
		}
		if err := r.loadChunk(ctx); err != nil {
			return intermediateRecord{}, err
		}
	}
	record := r.records[r.recordIndex]
	r.recordIndex++
	r.read++
	return record, nil
}

func (r *documentRunReader) loadChunk(ctx context.Context) error {
	chunk := r.ref.Chunks[r.chunkIndex]
	docs, err := r.store.client.Docs(ctx, &entroq.DocQuery{
		Namespace: r.ref.Namespace,
		IDs:       []string{chunk.ID},
	})
	if err != nil {
		return fmt.Errorf("read document run chunk %q: %w", chunk.ID, err)
	}
	if len(docs) == 0 {
		return fmt.Errorf("read document run chunk %q: not found", chunk.ID)
	}
	if docs[0].Version != chunk.Version {
		return fmt.Errorf("read document run chunk %q: version is %d, pointer requires %d", chunk.ID, docs[0].Version, chunk.Version)
	}
	var records []intermediateRecord
	if err := json.Unmarshal(docs[0].Content, &records); err != nil {
		return fmt.Errorf("parse document run chunk %q: %w", chunk.ID, err)
	}
	r.chunkIndex++
	r.records = records
	r.recordIndex = 0
	return nil
}

func (r *documentRunReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.records = nil
	return nil
}

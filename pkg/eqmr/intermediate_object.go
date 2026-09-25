package eqmr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shiblon/entroq"
)

type objectIntermediateStore struct {
	storeName string
	objects   objectStore
}

func newObjectIntermediateStore(name string, objects objectStore) (*objectIntermediateStore, error) {
	if name == "" {
		return nil, fmt.Errorf("eqmr intermediate object store: name is required")
	}
	if objects == nil {
		return nil, fmt.Errorf("eqmr intermediate object store %q: nil object store", name)
	}
	return &objectIntermediateStore{storeName: name, objects: objects}, nil
}

func (s *objectIntermediateStore) name() string { return s.storeName }

type objectIntermediateRunRef struct {
	Object  json.RawMessage `json:"object"`
	Records int             `json:"records"`
}

func (s *objectIntermediateStore) put(ctx context.Context, records []intermediateRecord) (json.RawMessage, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("put object run: no records")
	}
	stamp := time.Now().UTC().Format("20060102-150405.000000000")
	objectID := fmt.Sprintf("run-%s-%s", stamp, entroq.GenHex16())
	reader, writer := io.Pipe()
	encoded := make(chan error, 1)
	go func() {
		err := encodeIntermediateRun(writer, records)
		_ = writer.CloseWithError(err)
		encoded <- err
	}()

	objectRef, putErr := s.objects.put(ctx, objectID, reader)
	_ = reader.CloseWithError(putErr)
	encodeErr := <-encoded
	if encodeErr != nil {
		if putErr != nil {
			return nil, errors.Join(
				fmt.Errorf("encode object run %q: %w", objectID, encodeErr),
				fmt.Errorf("put object run %q: %w", objectID, putErr),
			)
		}
		deleteErr := s.objects.delete(context.WithoutCancel(ctx), objectRef)
		return nil, errors.Join(
			fmt.Errorf("encode object run %q: %w", objectID, encodeErr),
			deleteErr,
		)
	}
	if putErr != nil {
		return nil, fmt.Errorf("put object run %q: %w", objectID, putErr)
	}
	raw, err := json.Marshal(objectIntermediateRunRef{Object: objectRef, Records: len(records)})
	if err != nil {
		deleteErr := s.objects.delete(context.WithoutCancel(ctx), objectRef)
		return nil, errors.Join(
			fmt.Errorf("marshal object run %q reference: %w", objectID, err),
			deleteErr,
		)
	}
	return raw, nil
}

func (s *objectIntermediateStore) open(ctx context.Context, raw json.RawMessage) (intermediateReader, error) {
	ref, err := decodeObjectIntermediateRunRef(raw)
	if err != nil {
		return nil, err
	}
	body, err := s.objects.open(ctx, ref.Object)
	if err != nil {
		return nil, fmt.Errorf("open object run: %w", err)
	}
	reader, err := newEncodedIntermediateReader(body, ref.Records)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open object run: %w", err), body.Close())
	}
	return reader, nil
}

func (s *objectIntermediateStore) delete(ctx context.Context, raw json.RawMessage) error {
	ref, err := decodeObjectIntermediateRunRef(raw)
	if err != nil {
		return err
	}
	if err := s.objects.delete(ctx, ref.Object); err != nil {
		return fmt.Errorf("delete object run: %w", err)
	}
	return nil
}

func decodeObjectIntermediateRunRef(raw json.RawMessage) (objectIntermediateRunRef, error) {
	var ref objectIntermediateRunRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return ref, fmt.Errorf("parse object run reference: %w", err)
	}
	if ref.Records <= 0 || len(ref.Object) == 0 || !json.Valid(ref.Object) || bytes.Equal(bytes.TrimSpace(ref.Object), []byte("null")) {
		return ref, fmt.Errorf("invalid object run reference: object and positive record count are required")
	}
	return ref, nil
}

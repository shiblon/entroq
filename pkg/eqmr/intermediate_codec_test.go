package eqmr

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntermediateRunCodecRoundTrip(t *testing.T) {
	t.Parallel()
	records := []intermediateRecord{
		{Primary: "", Secondary: "", Value: ""},
		{Primary: "a", Secondary: "", Value: "one"},
		{Primary: "a", Secondary: "b", Value: "two"},
		{Primary: "snowman-☃", Secondary: "二", Value: "three"},
	}
	var encoded bytes.Buffer
	if err := encodeIntermediateRun(&encoded, records); err != nil {
		t.Fatalf("encode: %v", err)
	}
	const wantEncoding = "45514d5252554e010100000001010003616f6e6501010103616274776f010b0305736e6f776d616e2de29883e4ba8c74687265650004ad35ddcb62916c0c"
	if got := hex.EncodeToString(encoded.Bytes()); got != wantEncoding {
		t.Fatalf("encoding = %s, want %s", got, wantEncoding)
	}

	reader, err := newEncodedIntermediateReader(io.NopCloser(bytes.NewReader(encoded.Bytes())), len(records))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	got, err := readAllIntermediate(context.Background(), reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(got) != len(records) {
		t.Fatalf("read %d records, want %d", len(got), len(records))
	}
	for i := range records {
		if got[i] != records[i] {
			t.Fatalf("record %d = %#v, want %#v", i, got[i], records[i])
		}
	}
	if _, err := reader.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after footer = %v, want EOF", err)
	}
}

func TestIntermediateRunCodecRejectsInvalidStreams(t *testing.T) {
	t.Parallel()
	records := []intermediateRecord{{Primary: "a", Secondary: "b", Value: "value"}}
	var valid bytes.Buffer
	if err := encodeIntermediateRun(&valid, records); err != nil {
		t.Fatalf("encode: %v", err)
	}

	corrupt := append([]byte(nil), valid.Bytes()...)
	valueAt := bytes.Index(corrupt, []byte("value"))
	if valueAt < 0 {
		t.Fatal("encoded value not found")
	}
	corrupt[valueAt] ^= 0xff

	for _, tc := range []struct {
		name          string
		data          []byte
		expectedCount int
		want          string
	}{
		{name: "checksum", data: corrupt, expectedCount: len(records), want: "checksum"},
		{name: "truncated footer", data: valid.Bytes()[:valid.Len()-1], expectedCount: len(records), want: "footer checksum"},
		{name: "trailing data", data: append(append([]byte(nil), valid.Bytes()...), 1), expectedCount: len(records), want: "data after footer"},
		{name: "pointer count", data: valid.Bytes(), expectedCount: len(records) + 1, want: "pointer requires 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := newEncodedIntermediateReader(io.NopCloser(bytes.NewReader(tc.data)), tc.expectedCount)
			if err != nil {
				t.Fatalf("new reader: %v", err)
			}
			_, err = readAllIntermediate(context.Background(), reader)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("read error = %v, want %q", err, tc.want)
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
}

func TestObjectIntermediateStoreOverHTTP(t *testing.T) {
	t.Parallel()
	driver := newTestHTTPObjectDriver(t)
	server := httptest.NewServer(driver)
	t.Cleanup(server.Close)
	objects, err := newHTTPObjectStore(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("new HTTP store: %v", err)
	}
	store, err := newObjectIntermediateStore("test-http", objects)
	if err != nil {
		t.Fatalf("new intermediate store: %v", err)
	}
	records := []intermediateRecord{
		{Primary: "a", Secondary: "", Value: "one"},
		{Primary: "a", Secondary: "b", Value: "two"},
		{Primary: "z", Secondary: "", Value: "three"},
	}
	ctx := context.Background()
	ref, err := store.put(ctx, records)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	reader, err := store.open(ctx, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, readErr := readAllIntermediate(ctx, reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(records) {
		t.Fatalf("read %d records, want %d", len(got), len(records))
	}
	for i := range records {
		if got[i] != records[i] {
			t.Fatalf("record %d = %#v, want %#v", i, got[i], records[i])
		}
	}
	if err := store.delete(ctx, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.open(ctx, ref); err == nil {
		t.Fatal("open deleted run succeeded")
	}
}

func readAllIntermediate(ctx context.Context, reader intermediateReader) ([]intermediateRecord, error) {
	var records []intermediateRecord
	for {
		record, err := reader.Next(ctx)
		if err == io.EOF {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
}

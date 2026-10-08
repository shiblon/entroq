package eqmr

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shiblon/entroq/pkg/worker"
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
	const wantEncoding = "45514d5252554e02010428000000010003616f6e65010103616274776f0b0305736e6f776d616e2de29883e4ba8c7468726565c444abf13559b528000440e35043e1a3975f"
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

	// Wrong bytes are corrupt, and permanent, and so is a run that is short
	// although its source ended cleanly. A source that broke off is not:
	// reading again may get the whole run, so that is retried.
	for _, tc := range []struct {
		name          string
		data          []byte
		expectedCount int
		want          string
		corrupt       bool
	}{
		{name: "checksum", data: corrupt, expectedCount: len(records), want: "checksum", corrupt: true},
		{name: "truncated footer", data: valid.Bytes()[:valid.Len()-1], expectedCount: len(records), want: "ends early", corrupt: true},
		{name: "trailing data", data: append(append([]byte(nil), valid.Bytes()...), 1), expectedCount: len(records), want: "data after footer", corrupt: true},
		{name: "pointer count", data: valid.Bytes(), expectedCount: len(records) + 1, want: "pointer requires 2", corrupt: true},
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
			if got := errors.Is(err, errCorruptRun); got != tc.corrupt {
				t.Errorf("corrupt = %v, want %v for %v", got, tc.corrupt, err)
			}
			_, moved := worker.AsMove(runReadFailure(err, "read"))
			if moved != tc.corrupt {
				t.Errorf("quarantined = %v, want %v: only corruption skips retries", moved, tc.corrupt)
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
}

// brokenSource delivers part of a run and then fails the way a dropped
// connection does.
type brokenSource struct{ r io.Reader }

func (b brokenSource) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// TestIntermediateRunBrokenSourceIsRetried pins the other side: a run cut off
// because its source broke is not called corrupt, since reading again may get
// all of it.
func TestIntermediateRunBrokenSourceIsRetried(t *testing.T) {
	var valid bytes.Buffer
	if err := encodeIntermediateRun(&valid, []intermediateRecord{{Primary: "a", Value: "value"}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	cut := valid.Bytes()[:valid.Len()-3]
	reader, err := newEncodedIntermediateReader(io.NopCloser(brokenSource{bytes.NewReader(cut)}), 1)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	_, err = readAllIntermediate(context.Background(), reader)
	if err == nil || errors.Is(err, errCorruptRun) {
		t.Fatalf("read of a broken source: got %v, want a retryable error", err)
	}
	if _, retried := worker.AsRetry(runReadFailure(err, "read")); !retried {
		t.Errorf("a broken source was not retried")
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
	store, err := newObjectIntermediateStore(Descriptor{
		Name:     "test-http",
		Driver:   "test-http/1",
		Identity: server.URL,
	}, objects)
	if err != nil {
		t.Fatalf("new intermediate store: %v", err)
	}
	records := []intermediateRecord{
		{Primary: "a", Secondary: "", Value: "one"},
		{Primary: "a", Secondary: "b", Value: "two"},
		{Primary: "z", Secondary: "", Value: "three"},
	}
	ctx := context.Background()
	ref, err := store.put(ctx, "/codec-test", records)
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

// TestIntermediateRunDamageIsCaughtBeforeUse pins why runs are written in
// checksummed blocks: a reader verifies each block before yielding any of its
// records, so records from intact blocks arrive, and none from a damaged block
// ever reach the code consuming them.
func TestIntermediateRunDamageIsCaughtBeforeUse(t *testing.T) {
	ctx := context.Background()
	value := strings.Repeat("v", 1000)
	var records []intermediateRecord
	for i := range 300 { // about 300KB, several blocks
		records = append(records, intermediateRecord{Primary: fmt.Sprintf("k%06d", i), Value: value})
	}
	var encoded bytes.Buffer
	if err := encodeIntermediateRun(&encoded, records); err != nil {
		t.Fatalf("encode: %v", err)
	}

	reader, err := newEncodedIntermediateReader(io.NopCloser(bytes.NewReader(encoded.Bytes())), len(records))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	got, err := readAllIntermediate(ctx, reader)
	if err != nil || len(got) != len(records) {
		t.Fatalf("read intact multi-block run: %d records, %v", len(got), err)
	}

	// Damage a value two thirds of the way in, well past the first block.
	damaged := append([]byte(nil), encoded.Bytes()...)
	at := len(damaged) * 2 / 3
	for damaged[at] != 'v' {
		at++
	}
	damaged[at] = 'X'

	reader, err = newEncodedIntermediateReader(io.NopCloser(bytes.NewReader(damaged)), len(records))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	var yielded int
	for {
		record, err := reader.Next(ctx)
		if err != nil {
			if !errors.Is(err, errCorruptRun) {
				t.Fatalf("damaged run: got %v, want corruption", err)
			}
			break
		}
		if strings.Contains(record.Value, "X") {
			t.Fatalf("record %q from the damaged block was yielded", record.Primary)
		}
		yielded++
	}
	if yielded == 0 || yielded >= len(records) {
		t.Errorf("yielded %d of %d records before the damaged block; want the intact blocks only", yielded, len(records))
	}
}

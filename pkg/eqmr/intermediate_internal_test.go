package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

type testRunStore struct {
	runs [][]intermediateRecord
}

type testRunReader struct {
	records []intermediateRecord
	index   int
}

func (r *testRunReader) Next(context.Context) (intermediateRecord, error) {
	if r.index >= len(r.records) {
		return intermediateRecord{}, io.EOF
	}
	record := r.records[r.index]
	r.index++
	return record, nil
}
func (r *testRunReader) Close() error { return nil }

func (s *testRunStore) name() string { return "test" }
func (s *testRunStore) put(_ context.Context, records []intermediateRecord) (json.RawMessage, error) {
	id := len(s.runs)
	s.runs = append(s.runs, append([]intermediateRecord(nil), records...))
	return json.Marshal(id)
}
func (s *testRunStore) open(_ context.Context, raw json.RawMessage) (intermediateReader, error) {
	var id int
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	if id < 0 || id >= len(s.runs) {
		return nil, fmt.Errorf("run %d does not exist", id)
	}
	records := append([]intermediateRecord(nil), s.runs[id]...)
	return &testRunReader{records: records}, nil
}

func TestIntermediateSinkDoesNotPublishEmptyCombinedRun(t *testing.T) {
	ctx := context.Background()
	store := new(testRunStore)
	drop := func(context.Context, string, []string) ([]string, error) { return nil, nil }
	sink := newIntermediateSink(store, 1, 100, drop)
	if err := sink.write(ctx, "key", "", "value"); err != nil {
		t.Fatalf("write: %v", err)
	}
	runs, err := sink.finish(ctx)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(runs) != 0 || len(store.runs) != 0 {
		t.Fatalf("empty combiner output published %d pointers and %d runs", len(runs), len(store.runs))
	}
}

func TestIntermediateSinkRecutsExpandedCombinerOutput(t *testing.T) {
	ctx := context.Background()
	store := new(testRunStore)
	expand := func(context.Context, string, []string) ([]string, error) {
		return []string{
			strings.Repeat("a", 100),
			strings.Repeat("b", 100),
			strings.Repeat("c", 100),
		}, nil
	}
	sink := newIntermediateSink(store, 1, 200, expand)
	if err := sink.write(ctx, "key", "", "value"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sink.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(store.runs) < 2 {
		t.Fatalf("got %d run, want expanded output recut across runs", len(store.runs))
	}
	for i, run := range store.runs {
		size := 0
		for _, record := range run {
			size += intermediateRecordBytes(record)
		}
		if size > 200 && len(run) > 1 {
			t.Errorf("run %d is %d bytes, want at most 200 except for one oversized record", i, size)
		}
	}
}
func (s *testRunStore) delete(context.Context, json.RawMessage) error { return nil }

func TestIntermediateSinkCutsSortedRuns(t *testing.T) {
	ctx := context.Background()
	store := new(testRunStore)
	sink := newIntermediateSink(store, 1, 100, nil)

	input := []intermediateRecord{
		{Primary: "gamma", Value: "3"},
		{Primary: "alpha", Value: "4"},
		{Primary: "beta", Value: "2"},
		{Primary: "alpha", Value: "1"},
		{Primary: "delta", Value: "5"},
		{Primary: "beta", Value: "0"},
	}
	for _, record := range input {
		if err := sink.write(ctx, record.Primary, record.Secondary, record.Value); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	runs, err := sink.finish(ctx)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(runs) < 2 {
		t.Fatalf("got %d run, want multiple runs from the small threshold", len(runs))
	}

	total := 0
	for i, run := range store.runs {
		total += len(run)
		if !isSortedRun(run) {
			t.Errorf("run %d is not sorted: %+v", i, run)
		}
	}
	if total != len(input) {
		t.Errorf("stored %d records, want %d", total, len(input))
	}
}

func isSortedRun(run []intermediateRecord) bool {
	for i := 1; i < len(run); i++ {
		if recordLess(run[i], run[i-1]) {
			return false
		}
	}
	return true
}

func TestReduceIntermediateDrainsUnreadValues(t *testing.T) {
	ctx := context.Background()
	store := &testRunStore{runs: [][]intermediateRecord{
		{{Primary: "alpha", Value: "1"}, {Primary: "beta", Value: "2"}},
		{{Primary: "alpha", Value: "0"}, {Primary: "beta", Value: "3"}},
	}}
	runs := []intermediateRun{
		{Store: store.name(), Ref: json.RawMessage("0")},
		{Store: store.name(), Ref: json.RawMessage("1")},
	}
	merged, err := openMergedIntermediate(ctx, store, runs)
	if err != nil {
		t.Fatalf("open merge: %v", err)
	}
	defer func() {
		if err := merged.Close(); err != nil {
			t.Errorf("close merge: %v", err)
		}
	}()

	out, err := reduceIntermediate(ctx, merged, EmptyReducer)
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d outputs, want 2", len(out))
	}
	got := []string{out[0].Key, out[1].Key}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestDocumentIntermediateReaderLoadsOneChunkAtATime(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	store := &documentIntermediateStore{client: eq, namespace: "/intermediate-test"}
	records := make([]intermediateRecord, 2000)
	for i := range records {
		records[i] = intermediateRecord{
			Primary: fmt.Sprintf("key-%06d", i),
			Value:   strings.Repeat("x", 100),
		}
	}
	raw, err := store.put(ctx, records)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	ref, err := decodeDocumentRunRef(raw)
	if err != nil {
		t.Fatalf("decode reference: %v", err)
	}
	if len(ref.Chunks) < 2 {
		t.Fatalf("got %d chunk, want multiple chunks", len(ref.Chunks))
	}

	reader, err := store.open(ctx, raw)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	docReader := reader.(*documentRunReader)
	if len(docReader.records) != 0 {
		t.Fatalf("open loaded %d records before the first read", len(docReader.records))
	}
	if _, err := reader.Next(ctx); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if len(docReader.records) >= len(records) {
		t.Fatalf("first read loaded all %d records", len(docReader.records))
	}

	count := 1
	for {
		_, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read record %d: %v", count, err)
		}
		count++
	}
	if count != len(records) {
		t.Errorf("read %d records, want %d", count, len(records))
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.delete(ctx, raw); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestDocumentRunReferenceAcceptsInitialVersionZero(t *testing.T) {
	raw := json.RawMessage(`{
		"namespace":"run",
		"chunks":[{"id":"chunk","version":0}],
		"records":1
	}`)
	if _, err := decodeDocumentRunRef(raw); err != nil {
		t.Fatalf("decode initial version zero: %v", err)
	}
}

func TestDocumentRunReferenceRejectsNegativeVersion(t *testing.T) {
	raw := json.RawMessage(`{
		"namespace":"run",
		"chunks":[{"id":"chunk","version":-1}],
		"records":1
	}`)
	if _, err := decodeDocumentRunRef(raw); err == nil {
		t.Fatal("decode negative version succeeded")
	}
}

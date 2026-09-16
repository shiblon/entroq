package eqmr

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// DefaultIntermediateRunBytes bounds the mapper memory used to assemble a
// sorted intermediate run. A single record may exceed the bound.
const DefaultIntermediateRunBytes = 1 << 20

// intermediateRecord is the storage-level map output. Secondary is empty for
// the current Mapper API, but remains a real sort dimension in the storage
// contract so sources can add secondary sort without changing the shuffle.
type intermediateRecord struct {
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
	Value     string `json:"value"`
}

// intermediateRun is the durable pointer published by a mapper. Ref is opaque
// to the pipeline and interpreted only by the named store.
type intermediateRun struct {
	Partition int             `json:"partition"`
	Store     string          `json:"store"`
	Ref       json.RawMessage `json:"ref"`
}

// intermediateReader streams one sorted immutable run.
type intermediateReader interface {
	Next(context.Context) (intermediateRecord, error)
	Close() error
}

// intermediateStore persists and opens immutable sorted runs. put receives a
// complete bounded run; readers expose records one at a time so file-backed
// stores need not materialize the run when they are added.
type intermediateStore interface {
	name() string
	put(context.Context, []intermediateRecord) (json.RawMessage, error)
	open(context.Context, json.RawMessage) (intermediateReader, error)
	delete(context.Context, json.RawMessage) error
}

// intermediateSink owns partitioning, buffering, sorting, combining, and run
// cutting for one map attempt. Runs are durable before finish returns, but are
// invisible to reducers until the mapper publishes their pointers atomically
// with its split deletion.
type intermediateSink struct {
	store      intermediateStore
	partitions int
	maxBytes   int
	combiner   Combiner

	buffers [][]intermediateRecord
	sizes   []int
	total   int
	runs    []intermediateRun
	err     error
}

func newIntermediateSink(store intermediateStore, partitions, maxBytes int, combiner Combiner) *intermediateSink {
	if maxBytes <= 0 {
		maxBytes = DefaultIntermediateRunBytes
	}
	return &intermediateSink{
		store:      store,
		partitions: partitions,
		maxBytes:   maxBytes,
		combiner:   combiner,
		buffers:    make([][]intermediateRecord, partitions),
		sizes:      make([]int, partitions),
	}
}

func (s *intermediateSink) write(ctx context.Context, primary, secondary, value string) error {
	if s.err != nil {
		return s.err
	}
	p := ShardForKey(primary, s.partitions)
	record := intermediateRecord{Primary: primary, Secondary: secondary, Value: value}
	s.buffers[p] = append(s.buffers[p], record)
	n := intermediateRecordBytes(record)
	s.sizes[p] += n
	s.total += n
	if s.total >= s.maxBytes {
		s.err = s.flushLargest(ctx)
	}
	return s.err
}

func intermediateRecordBytes(record intermediateRecord) int {
	// The fixed allowance covers slice/string headers and JSON punctuation. The
	// threshold is a memory bound, not a serialized-size promise.
	return len(record.Primary) + len(record.Secondary) + len(record.Value) + 64
}

func (s *intermediateSink) flushLargest(ctx context.Context) error {
	partition := -1
	for p, size := range s.sizes {
		if size > 0 && (partition < 0 || size > s.sizes[partition]) {
			partition = p
		}
	}
	if partition < 0 {
		return nil
	}
	return s.flush(ctx, partition)
}

func (s *intermediateSink) flush(ctx context.Context, partition int) error {
	records := s.buffers[partition]
	if len(records) == 0 {
		return nil
	}
	sort.Slice(records, func(i, j int) bool { return recordLess(records[i], records[j]) })

	if s.combiner != nil {
		combined, err := combineRun(ctx, records, s.combiner)
		if err != nil {
			return err
		}
		records = combined
		sort.Slice(records, func(i, j int) bool { return recordLess(records[i], records[j]) })
	}
	if len(records) == 0 {
		s.clearBuffer(partition)
		return nil
	}

	for _, runRecords := range cutIntermediateRecords(records, s.maxBytes) {
		ref, err := s.store.put(ctx, runRecords)
		if err != nil {
			return fmt.Errorf("write intermediate run for partition %d: %w", partition, err)
		}
		s.runs = append(s.runs, intermediateRun{
			Partition: partition,
			Store:     s.store.name(),
			Ref:       ref,
		})
	}
	s.clearBuffer(partition)
	return nil
}

func (s *intermediateSink) clearBuffer(partition int) {
	s.total -= s.sizes[partition]
	s.buffers[partition] = nil
	s.sizes[partition] = 0
}

func recordLess(a, b intermediateRecord) bool {
	if a.Primary != b.Primary {
		return a.Primary < b.Primary
	}
	if a.Secondary != b.Secondary {
		return a.Secondary < b.Secondary
	}
	return a.Value < b.Value
}

func cutIntermediateRecords(records []intermediateRecord, maxBytes int) [][]intermediateRecord {
	var runs [][]intermediateRecord
	start, size := 0, 0
	for i, record := range records {
		recordSize := intermediateRecordBytes(record)
		if i > start && size+recordSize > maxBytes {
			runs = append(runs, records[start:i])
			start, size = i, 0
		}
		size += recordSize
	}
	if start < len(records) {
		runs = append(runs, records[start:])
	}
	return runs
}

func combineRun(ctx context.Context, records []intermediateRecord, combiner Combiner) ([]intermediateRecord, error) {
	out := make([]intermediateRecord, 0, len(records))
	for start := 0; start < len(records); {
		end := start + 1
		for end < len(records) && records[end].Primary == records[start].Primary {
			if records[end].Secondary != records[start].Secondary {
				return nil, fmt.Errorf("combine %q: secondary keys require a secondary-aware combiner", records[start].Primary)
			}
			end++
		}
		values := make([]string, end-start)
		for i := start; i < end; i++ {
			values[i-start] = records[i].Value
		}
		values, err := combiner(ctx, records[start].Primary, values)
		if err != nil {
			return nil, fmt.Errorf("combine %q: %w", records[start].Primary, err)
		}
		for _, value := range values {
			if err := ValidateText(fmt.Sprintf("combined value for key %q", records[start].Primary), value); err != nil {
				return nil, err
			}
			out = append(out, intermediateRecord{
				Primary:   records[start].Primary,
				Secondary: records[start].Secondary,
				Value:     value,
			})
		}
		start = end
	}
	return out, nil
}

func (s *intermediateSink) finish(ctx context.Context) ([]intermediateRun, error) {
	if s.err != nil {
		return nil, s.err
	}
	for partition := range s.partitions {
		if err := s.flush(ctx, partition); err != nil {
			s.err = err
			return nil, err
		}
	}
	return append([]intermediateRun(nil), s.runs...), nil
}

func (s *intermediateSink) abort(ctx context.Context) error {
	var errs []error
	for _, run := range s.runs {
		if err := s.store.delete(ctx, run.Ref); err != nil {
			errs = append(errs, fmt.Errorf("delete partition %d run: %w", run.Partition, err))
		}
	}
	return errors.Join(errs...)
}

// mergedIntermediate is a heap merge across sorted immutable runs.
type mergedIntermediate struct {
	readers []intermediateReader
	heap    intermediateHeap
}

type intermediateCursor struct {
	record intermediateRecord
	reader intermediateReader
	order  int
}

type intermediateHeap []*intermediateCursor

func (h intermediateHeap) Len() int { return len(h) }
func (h intermediateHeap) Less(i, j int) bool {
	if recordLess(h[i].record, h[j].record) {
		return true
	}
	if recordLess(h[j].record, h[i].record) {
		return false
	}
	return h[i].order < h[j].order
}
func (h intermediateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *intermediateHeap) Push(x any)   { *h = append(*h, x.(*intermediateCursor)) }
func (h *intermediateHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

func openMergedIntermediate(ctx context.Context, store intermediateStore, runs []intermediateRun) (*mergedIntermediate, error) {
	merged := new(mergedIntermediate)
	for i, run := range runs {
		if run.Store != store.name() {
			return nil, errors.Join(
				fmt.Errorf("open intermediate run: store %q is not configured store %q", run.Store, store.name()),
				merged.Close(),
			)
		}
		reader, err := store.open(ctx, run.Ref)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("open intermediate run %d: %w", i, err),
				merged.Close(),
			)
		}
		merged.readers = append(merged.readers, reader)
		record, err := reader.Next(ctx)
		switch {
		case err == nil:
			heap.Push(&merged.heap, &intermediateCursor{record: record, reader: reader, order: i})
		case errors.Is(err, io.EOF):
		case err != nil:
			return nil, errors.Join(
				fmt.Errorf("read intermediate run %d: %w", i, err),
				merged.Close(),
			)
		}
	}
	return merged, nil
}

func (m *mergedIntermediate) Next(ctx context.Context) (intermediateRecord, error) {
	if len(m.heap) == 0 {
		return intermediateRecord{}, io.EOF
	}
	cursor := heap.Pop(&m.heap).(*intermediateCursor)
	record := cursor.record
	next, err := cursor.reader.Next(ctx)
	switch {
	case err == nil:
		cursor.record = next
		heap.Push(&m.heap, cursor)
	case errors.Is(err, io.EOF):
	case err != nil:
		return intermediateRecord{}, err
	}
	return record, nil
}

func (m *mergedIntermediate) Close() error {
	var errs []error
	for _, reader := range m.readers {
		if err := reader.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.readers = nil
	m.heap = nil
	return errors.Join(errs...)
}

// groupedIntermediate adapts the merged record stream to the existing
// ReducerInput API without collecting all values for a primary key.
type groupedIntermediate struct {
	ctx    context.Context
	merged *mergedIntermediate
	peek   intermediateRecord
	have   bool
	err    error
}

func newGroupedIntermediate(ctx context.Context, merged *mergedIntermediate) *groupedIntermediate {
	return &groupedIntermediate{ctx: ctx, merged: merged}
}

func (g *groupedIntermediate) load() bool {
	if g.have {
		return true
	}
	if g.err != nil {
		return false
	}
	record, err := g.merged.Next(g.ctx)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			g.err = err
		}
		return false
	}
	g.peek = record
	g.have = true
	return true
}

func (g *groupedIntermediate) nextGroup() (*intermediateGroup, bool) {
	if !g.load() {
		return nil, false
	}
	return &intermediateGroup{stream: g, key: g.peek.Primary}, true
}

type intermediateGroup struct {
	stream  *groupedIntermediate
	key     string
	current intermediateRecord
}

func (g *intermediateGroup) Key() string { return g.key }
func (g *intermediateGroup) Value() string {
	return g.current.Value
}
func (g *intermediateGroup) Err() error { return g.stream.err }
func (g *intermediateGroup) Next() bool {
	if !g.stream.load() || g.stream.peek.Primary != g.key {
		return false
	}
	g.current = g.stream.peek
	g.stream.have = false
	return true
}

func (g *intermediateGroup) drain() error {
	for g.Next() {
	}
	return g.Err()
}

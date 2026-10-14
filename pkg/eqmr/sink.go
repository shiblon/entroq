package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// Sink receives a run's output, one value per key, grouped by reduce
// partition, and owns its format, naming, checksums, and readiness.
//
// A reducer stages each attempt at a partition under a name no other attempt
// shares, and the controller commits one attempt per partition, so duplicate
// and failed attempts are never visible. Every method may be retried after a
// crash, and must be idempotent as documented. The run never deletes a staged
// attempt; the sink sweeps the uncommitted ones when the run ends.
//
// A Sink is configured in every process that writes or commits for a run, and
// the run names it by Descriptor. Errors are retried, except a
// worker.MoveError, which fails the run at once with its reason.
type Sink interface {
	// Descriptor identifies the sink. See Descriptor.
	Descriptor() Descriptor

	// Stage opens a writer for one attempt at one partition of run. Nothing it
	// writes is visible as output until it is committed.
	Stage(ctx context.Context, run string, partition int) (SinkWriter, error)

	// Commit makes a staged attempt the output of its partition. The first
	// commit for a partition wins: any later commit, of the same attempt or
	// another, leaves the committed output as it is and returns nil. The
	// controller holds the partition's claim while Commit runs, and every
	// acceptance waits on it.
	Commit(ctx context.Context, run string, partition int, staged json.RawMessage) error

	// Finish marks run's output complete, once every partition with data is
	// committed. It is told the partition count and which partitions received
	// no data. It must be safe to call more than once.
	Finish(ctx context.Context, run string, partitions int, empty []int) error

	// Sweep removes every uncommitted staged attempt for run, including those
	// whose writer died before reporting them. It is called once the run has
	// ended, after any Finish. It must never remove committed output, and
	// must be safe to call more than once.
	Sweep(ctx context.Context, run string) error
}

// SinkWriter writes one staged attempt at one partition. Keys arrive in
// sorted order, each exactly once.
type SinkWriter interface {
	Write(ctx context.Context, key, value string) error
	// Close finishes the attempt and returns an opaque reference to it, for
	// the controller to commit. It is called even when the attempt failed,
	// and Sweep collects what was staged.
	Close(ctx context.Context) (staged json.RawMessage, err error)
}

// sinks is the registry of sinks.
type sinks = *registry[Sink]

func newSinks(ss ...Sink) (sinks, error) {
	return newRegistry("sink", Sink.Descriptor, ss...)
}

const (
	documentSinkName    = "documents"
	documentSinkDriver  = "entroq.documents.sink/1"
	documentSinkStaging = "output/staged/"
	outputManifestKey   = "output/manifest"
	outputManifestID    = "output-manifest"
)

const outputPartPrefix = "output/part/"

func outputPartKey(p int) string { return fmt.Sprintf("%s%06d", outputPartPrefix, p) }
func outputPartID(p int) string  { return fmt.Sprintf("output-part-%06d", p) }

// documentSink keeps a run's output in the run's document namespace, for small
// jobs; Controller.Results reads it. A staged attempt is a set of chunk docs,
// and committing inserts the partition's part doc, pointing at them, under an
// explicit id, so the first commit wins. Finish inserts a manifest the same
// way.
type documentSink struct {
	client    *entroq.EntroQ
	namespace string
	staging   *documentIntermediateStore
}

func newDocumentSink(client *entroq.EntroQ, namespace string) *documentSink {
	return &documentSink{
		client:    client,
		namespace: namespace,
		staging:   &documentIntermediateStore{client: client, namespace: namespace, keyPrefix: documentSinkStaging},
	}
}

// documentStaged is a staged attempt. Chunks is nil for empty output.
type documentStaged struct {
	Chunks json.RawMessage `json:"chunks,omitempty"`
}

// documentManifest is the content of the manifest doc.
type documentManifest struct {
	Partitions int   `json:"partitions"`
	Empty      []int `json:"empty,omitempty"`
}

func (s *documentSink) Descriptor() Descriptor {
	return Descriptor{Name: documentSinkName, Driver: documentSinkDriver, Identity: fitText(s.namespace, maxDescriptorFieldBytes)}
}

// checkRun refuses, permanently, a run other than the one whose namespace this
// sink keeps.
func (s *documentSink) checkRun(run, what string) error {
	if run != s.namespace {
		return worker.MoveErrorf("document sink for %q cannot %s for run %q", s.namespace, what, run)
	}
	return nil
}

func (s *documentSink) Stage(_ context.Context, run string, _ int) (SinkWriter, error) {
	if err := s.checkRun(run, "stage output"); err != nil {
		return nil, err
	}
	return &documentSinkWriter{sink: s}, nil
}

func (s *documentSink) Commit(ctx context.Context, run string, partition int, staged json.RawMessage) error {
	if err := s.checkRun(run, "commit output"); err != nil {
		return err
	}
	_, err := s.client.Modify(ctx, entroq.PuttingDocInto(s.namespace,
		entroq.WithIDKeys(outputPartID(partition), outputPartKey(partition), ""),
		entroq.WithRawContent(staged),
	))
	return ignoreCollision(err)
}

func (s *documentSink) Finish(ctx context.Context, run string, partitions int, empty []int) error {
	if err := s.checkRun(run, "finish output"); err != nil {
		return err
	}
	_, err := s.client.Modify(ctx, entroq.PuttingDocInto(s.namespace,
		entroq.WithIDKeys(outputManifestID, outputManifestKey, ""),
		entroq.WithContent(documentManifest{Partitions: partitions, Empty: empty}),
	))
	return ignoreCollision(err)
}

// Sweep deletes every staged chunk doc that no part doc references.
func (s *documentSink) Sweep(ctx context.Context, run string) error {
	if err := s.checkRun(run, "sweep"); err != nil {
		return err
	}
	parts, err := s.client.Docs(ctx, &entroq.DocQuery{
		Namespace: s.namespace,
		KeyStart:  outputPartPrefix,
		KeyEnd:    prefixEnd(outputPartPrefix),
	})
	if err != nil {
		return fmt.Errorf("sweep document output: read parts: %w", err)
	}
	committed := make(map[string]bool)
	for _, part := range parts {
		st, err := decodeDocumentStaged(part.Content)
		if err != nil {
			return fmt.Errorf("sweep document output: part %q: %w", part.Key, err)
		}
		if st.Chunks == nil {
			continue
		}
		ref, err := decodeDocumentRunRef(st.Chunks)
		if err != nil {
			return fmt.Errorf("sweep document output: part %q: %w", part.Key, err)
		}
		for _, c := range ref.Chunks {
			committed[c.ID] = true
		}
	}
	staged, err := s.client.Docs(ctx, &entroq.DocQuery{
		Namespace:  s.namespace,
		KeyStart:   documentSinkStaging,
		KeyEnd:     prefixEnd(documentSinkStaging),
		OmitValues: true,
	})
	if err != nil {
		return fmt.Errorf("sweep document output: read staged chunks: %w", err)
	}
	var errs []error
	for _, d := range staged {
		if committed[d.ID] {
			continue
		}
		if _, err := s.client.Modify(ctx, d.Delete()); err != nil {
			if de, ok := entroq.AsDependency(err); ok && de.HasMissingDocs() {
				continue
			}
			errs = append(errs, fmt.Errorf("delete staged chunk %q: %w", d.ID, err))
		}
	}
	return errors.Join(errs...)
}

// ignoreCollision treats a collided explicit-id insert as success: the doc was
// already written.
func ignoreCollision(err error) error {
	if de, ok := entroq.AsDependency(err); ok && de.HasCollisions() && !de.HasMissing() && !de.HasMissingDocs() {
		return nil
	}
	return err
}

func decodeDocumentStaged(raw json.RawMessage) (documentStaged, error) {
	var st documentStaged
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("parse staged document output: %w", err)
	}
	return st, nil
}

// readPartition reads one committed partition. A partition with no part doc
// received no data.
func (s *documentSink) readPartition(ctx context.Context, partition int) ([]*KV, error) {
	docs, err := s.client.Docs(ctx, &entroq.DocQuery{Namespace: s.namespace, KeyExact: outputPartKey(partition)})
	if err != nil {
		return nil, fmt.Errorf("read output partition %d: %w", partition, err)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	st, err := decodeDocumentStaged(docs[0].Content)
	if err != nil {
		return nil, fmt.Errorf("output partition %d: %w", partition, err)
	}
	if st.Chunks == nil {
		return nil, nil
	}
	reader, err := s.staging.open(ctx, st.Chunks)
	if err != nil {
		return nil, fmt.Errorf("open output partition %d: %w", partition, err)
	}
	var out []*KV
	for {
		record, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("read output partition %d: %w", partition, err), reader.Close())
		}
		out = append(out, NewKV(record.Primary, record.Value))
	}
	return out, reader.Close()
}

// manifest reads the manifest, or reports that the output is not finished.
func (s *documentSink) manifest(ctx context.Context) (documentManifest, error) {
	docs, err := s.client.Docs(ctx, &entroq.DocQuery{Namespace: s.namespace, KeyExact: outputManifestKey})
	if err != nil {
		return documentManifest{}, fmt.Errorf("read output manifest: %w", err)
	}
	if len(docs) == 0 {
		return documentManifest{}, fmt.Errorf("output has no manifest: not finished")
	}
	m, err := entroq.GetContent[documentManifest](docs[0])
	if err != nil {
		return m, fmt.Errorf("parse output manifest: %w", err)
	}
	return m, nil
}

// documentSinkWriter buffers one attempt, the whole partition, and writes it as
// chunk docs on Close.
type documentSinkWriter struct {
	sink    *documentSink
	records []intermediateRecord
}

// Write keeps one result. Documents hold text, so a value that is not valid
// NUL-free UTF-8 is refused permanently.
func (w *documentSinkWriter) Write(_ context.Context, key, value string) error {
	if err := ValidateText(fmt.Sprintf("result value for key %q", key), value); err != nil {
		return worker.MoveErrorf("%v", err)
	}
	w.records = append(w.records, intermediateRecord{Primary: key, Value: value})
	return nil
}

func (w *documentSinkWriter) Close(ctx context.Context) (json.RawMessage, error) {
	st := documentStaged{}
	if len(w.records) > 0 {
		chunks, err := w.sink.staging.put(ctx, w.sink.namespace, w.records)
		if err != nil {
			return nil, fmt.Errorf("stage document output: %w", err)
		}
		st.Chunks = chunks
	}
	w.records = nil
	return json.Marshal(st)
}

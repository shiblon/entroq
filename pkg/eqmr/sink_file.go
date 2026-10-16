package eqmr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/shardname"
	"github.com/shiblon/entroq/pkg/worker"
)

// fileSinkDriver is NewFileSink's driver name and compatibility version.
const fileSinkDriver = "entroq.eqmr.filesink/1"

// NewFileSink returns a sink that writes partition P of R to the file
// shardname.Name(base, P, R), one line per record in format. base is an
// absolute path; name is the logical name the run records.
//
// Every partition has a file, empty if it received no data, so base@R names
// the whole output, and a file source of the same format reads it back. Finish
// writes base.manifest, a JSON list of the files, which marks the output
// complete.
//
// The output belongs to the first run that writes it: another run is refused
// until the files, the manifest, and the hidden .<name>.eqmr directory beside
// them are removed. Attempts are staged in that directory and committed by
// hard link, so the filesystem must support links. Every process that reduces
// or controls the run must see it at the same path. The sink is identified by
// base and format.
func NewFileSink(name, base string, format FileFormat) (Sink, error) {
	if format.line == nil {
		return nil, fmt.Errorf("eqmr file sink: format is required")
	}
	if !filepath.IsAbs(base) || filepath.Clean(base) != base || filepath.Base(base) == string(filepath.Separator) {
		return nil, fmt.Errorf("eqmr file sink: base %q is not a clean absolute file path", base)
	}
	meta := filepath.Join(filepath.Dir(base), "."+filepath.Base(base)+".eqmr")
	return &fileSink{
		desc: Descriptor{
			Name:     name,
			Driver:   fileSinkDriver,
			Identity: fitText("file://"+base+";format="+format.name, maxDescriptorFieldBytes),
		},
		base:   base,
		format: format,
		owner:  filepath.Join(meta, "owner"),
		staged: filepath.Join(meta, "staged"),
	}, nil
}

type fileSink struct {
	desc   Descriptor
	base   string
	format FileFormat
	// owner holds the name of the run the output belongs to.
	owner string
	// staged holds the staged attempts, which Sweep removes.
	staged string
}

// fileStaged is a staged attempt: a file in the staging directory.
type fileStaged struct {
	Name       string `json:"name"`
	Partition  int    `json:"partition"`
	Partitions int    `json:"partitions"`
}

// fileManifest is the content of base.manifest.
type fileManifest struct {
	Run    string   `json:"run"`
	Format string   `json:"format"`
	Files  []string `json:"files"`
	Empty  []int    `json:"empty,omitempty"`
}

func (s *fileSink) Descriptor() Descriptor { return s.desc }

// claim makes the output run's, or refuses, permanently, if it is another's.
func (s *fileSink) claim(run string) error {
	if err := os.MkdirAll(filepath.Dir(s.owner), 0o755); err != nil {
		return fmt.Errorf("eqmr file sink: %w", err)
	}
	// Linked into place whole, so the owner is never seen half written.
	err := writeExclusive(filepath.Dir(s.owner), s.owner, []byte(run))
	if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("eqmr file sink: record owner: %w", err)
	}
	got, err := os.ReadFile(s.owner)
	if err != nil {
		return fmt.Errorf("eqmr file sink: read owner: %w", err)
	}
	if string(got) != run {
		return worker.MoveErrorf("eqmr file sink: %s belongs to run %q; remove its output to reuse it", s.base, got)
	}
	return nil
}

func (s *fileSink) shard(partition, partitions int) string {
	return shardname.Name(s.base, partition, partitions)
}

func (s *fileSink) Stage(_ context.Context, run string, partition, partitions int) (SinkWriter, error) {
	if partitions < 1 || partition < 0 || partition >= partitions {
		return nil, worker.MoveErrorf("eqmr file sink: partition %d of %d", partition, partitions)
	}
	if err := s.claim(run); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.staged, 0o755); err != nil {
		return nil, fmt.Errorf("eqmr file sink: %w", err)
	}
	ref := fileStaged{
		Name:       fmt.Sprintf("p%06d-%s", partition, entroq.GenHex16()),
		Partition:  partition,
		Partitions: partitions,
	}
	f, err := os.OpenFile(filepath.Join(s.staged, ref.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eqmr file sink: stage: %w", err)
	}
	return &fileSinkWriter{sink: s, ref: ref, f: f, w: bufio.NewWriter(f)}, nil
}

// Commit links the staged file to its shard name. A shard that exists was
// committed first, by this run, since the run owns the output.
func (s *fileSink) Commit(_ context.Context, run string, partition int, raw json.RawMessage) error {
	var ref fileStaged
	if err := json.Unmarshal(raw, &ref); err != nil {
		return worker.MoveErrorf("eqmr file sink: parse staged attempt: %v", err)
	}
	if ref.Partition != partition || ref.Name != filepath.Base(ref.Name) || !filepath.IsLocal(ref.Name) {
		return worker.MoveErrorf("eqmr file sink: staged attempt %q is not one of partition %d", ref.Name, partition)
	}
	if err := s.claim(run); err != nil {
		return err
	}
	shard := s.shard(partition, ref.Partitions)
	err := os.Link(filepath.Join(s.staged, ref.Name), shard)
	switch {
	case err == nil:
		return syncDir(filepath.Dir(shard))
	case errors.Is(err, os.ErrExist):
		return nil
	case errors.Is(err, os.ErrNotExist):
		if _, statErr := os.Stat(shard); statErr == nil {
			return nil
		}
		return worker.MoveErrorf("eqmr file sink: commit partition %d: %v", partition, err)
	default:
		return fmt.Errorf("eqmr file sink: commit partition %d: %w", partition, err)
	}
}

// Finish writes an empty shard for each partition with no data, then the
// manifest.
func (s *fileSink) Finish(_ context.Context, run string, partitions int, empty []int) error {
	if err := s.claim(run); err != nil {
		return err
	}
	if err := os.MkdirAll(s.staged, 0o755); err != nil {
		return fmt.Errorf("eqmr file sink: %w", err)
	}
	isEmpty := make(map[int]bool, len(empty))
	for _, p := range empty {
		isEmpty[p] = true
		err := writeExclusive(s.staged, s.shard(p, partitions), nil)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("eqmr file sink: empty partition %d: %w", p, err)
		}
	}
	m := fileManifest{Run: run, Format: s.format.name, Empty: empty}
	for p := range partitions {
		shard := s.shard(p, partitions)
		if !isEmpty[p] {
			_, err := os.Stat(shard)
			if errors.Is(err, os.ErrNotExist) {
				return worker.MoveErrorf("eqmr file sink: partition %d has no committed output", p)
			}
			if err != nil {
				return fmt.Errorf("eqmr file sink: partition %d: %w", p, err)
			}
		}
		m.Files = append(m.Files, filepath.Base(shard))
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("eqmr file sink: encode manifest: %w", err)
	}
	// Every call writes the same manifest, so the last may replace the first.
	manifest := s.base + ".manifest"
	tmp := filepath.Join(s.staged, "manifest-"+entroq.GenHex16())
	if err := writeSynced(tmp, body); err != nil {
		return fmt.Errorf("eqmr file sink: manifest: %w", err)
	}
	if err := os.Rename(tmp, manifest); err != nil {
		return errors.Join(fmt.Errorf("eqmr file sink: manifest: %w", err), os.Remove(tmp))
	}
	return syncDir(filepath.Dir(manifest))
}

// Sweep removes the staging directory of output run owns. Committed shards are
// links of their own, so removing the staged names leaves them in place. It
// claims nothing: a run that never staged has nothing here.
func (s *fileSink) Sweep(_ context.Context, run string) error {
	got, err := os.ReadFile(s.owner)
	if errors.Is(err, os.ErrNotExist) || (err == nil && string(got) != run) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("eqmr file sink: sweep: read owner: %w", err)
	}
	if err := os.RemoveAll(s.staged); err != nil {
		return fmt.Errorf("eqmr file sink: sweep: %w", err)
	}
	return nil
}

// fileSinkWriter writes one staged attempt.
type fileSinkWriter struct {
	sink *fileSink
	ref  fileStaged
	f    *os.File
	w    *bufio.Writer
}

func (w *fileSinkWriter) Write(_ context.Context, key, value string) error {
	line, err := w.sink.format.line(key, value)
	if err != nil {
		return err
	}
	if _, err := w.w.WriteString(line); err != nil {
		return fmt.Errorf("eqmr file sink: write: %w", err)
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("eqmr file sink: write: %w", err)
	}
	return nil
}

// Close syncs the attempt, so a committed shard is durable.
func (w *fileSinkWriter) Close(context.Context) (json.RawMessage, error) {
	err := errors.Join(w.w.Flush(), w.f.Sync())
	if closeErr := w.f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncDir(w.sink.staged)
	}
	if err != nil {
		return nil, fmt.Errorf("eqmr file sink: stage %s: %w", w.ref.Name, err)
	}
	return json.Marshal(w.ref)
}

// writeExclusive creates path holding body, or fails with os.ErrExist. The file
// is written and synced in tmpDir, on the same filesystem, then linked into
// place, so it never appears partly written.
func writeExclusive(tmpDir, path string, body []byte) error {
	tmp := filepath.Join(tmpDir, filepath.Base(path)+".tmp-"+entroq.GenHex16())
	if err := writeSynced(tmp, body); err != nil {
		return err
	}
	linkErr := os.Link(tmp, path)
	if err := errors.Join(linkErr, os.Remove(tmp)); err != nil {
		if errors.Is(linkErr, os.ErrExist) {
			return linkErr
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeSynced writes body to a new file at path and syncs it.
func writeSynced(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	err = errors.Join(err, f.Sync())
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

package eqmr

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
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
// writes base.manifest, listing each file with its SHA-256, records, and
// bytes, which marks the output complete.
//
// Attempts are staged in a hidden .<name>.eqmr directory beside the output and
// committed by rename. The output belongs to the first run that writes it:
// another run is refused until the files, the manifest, and that directory are
// removed. Every process that reduces or controls the run must see it at the
// same path. The sink is identified by base and format.
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
		parts:  filepath.Join(meta, "parts"),
		staged: filepath.Join(meta, "staged"),
	}, nil
}

type fileSink struct {
	desc   Descriptor
	base   string
	format FileFormat
	// owner holds the name of the run the output belongs to.
	owner string
	// parts holds the sums of each committed shard, for the manifest.
	parts string
	// staged holds the staged attempts, which Sweep removes.
	staged string
}

// fileSums describe a file's content.
type fileSums struct {
	SHA256  string `json:"sha256"`
	Records int64  `json:"records"`
	Bytes   int64  `json:"bytes"`
}

// emptySums are the sums of an empty file.
var emptySums = fileSums{SHA256: hex.EncodeToString(sha256.New().Sum(nil))}

// fileStaged is a staged attempt: a file in the staging directory.
type fileStaged struct {
	Name       string `json:"name"`
	Partition  int    `json:"partition"`
	Partitions int    `json:"partitions"`
	fileSums
}

// fileManifestEntry is one file of the manifest, named relative to it.
type fileManifestEntry struct {
	Name string `json:"name"`
	fileSums
}

// fileManifest is the content of base.manifest.
type fileManifest struct {
	Run    string              `json:"run"`
	Format string              `json:"format"`
	Files  []fileManifestEntry `json:"files"`
	Empty  []int               `json:"empty,omitempty"`
}

func (s *fileSink) Descriptor() Descriptor { return s.desc }

// claim makes the output run's, or refuses, permanently, if it is another's.
// Two runs claiming at once may both see their own name, but the first to
// commit after the other's claim is refused.
func (s *fileSink) claim(run string) error {
	ok, err := exists(s.owner)
	if err == nil && !ok {
		err = s.replace(s.owner, []byte(run))
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

func (s *fileSink) partSums(partition int) string {
	return filepath.Join(s.parts, fmt.Sprintf("p%06d.json", partition))
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
	w := &fileSinkWriter{sink: s, ref: ref, f: f, hash: sha256.New()}
	w.w = bufio.NewWriter(io.MultiWriter(f, w.hash))
	return w, nil
}

// Commit renames the staged file to its shard name, unless the shard exists.
// The controller holds the partition's claim, so commits of one partition never
// overlap, and a shard that exists was committed first. Its sums are recorded
// before the rename, so they always describe the shard in place.
func (s *fileSink) Commit(_ context.Context, run string, partition int, raw json.RawMessage) error {
	var ref fileStaged
	if err := json.Unmarshal(raw, &ref); err != nil {
		return worker.MoveErrorf("eqmr file sink: parse staged attempt: %v", err)
	}
	if ref.Partition != partition || ref.Partitions <= partition || ref.Name != filepath.Base(ref.Name) || !filepath.IsLocal(ref.Name) {
		return worker.MoveErrorf("eqmr file sink: staged attempt %q is not one of partition %d", ref.Name, partition)
	}
	if err := s.claim(run); err != nil {
		return err
	}
	shard := s.shard(partition, ref.Partitions)
	if done, err := exists(shard); done || err != nil {
		return err
	}
	staged := filepath.Join(s.staged, ref.Name)
	info, err := os.Stat(staged)
	if errors.Is(err, os.ErrNotExist) {
		return worker.MoveErrorf("eqmr file sink: commit partition %d: staged attempt %s is gone", partition, ref.Name)
	}
	if err != nil {
		return fmt.Errorf("eqmr file sink: commit partition %d: %w", partition, err)
	}
	if info.Size() != ref.Bytes {
		return worker.MoveErrorf("eqmr file sink: commit partition %d: staged attempt %s has %d bytes, want %d", partition, ref.Name, info.Size(), ref.Bytes)
	}
	sums, err := json.Marshal(ref.fileSums)
	if err != nil {
		return fmt.Errorf("eqmr file sink: encode sums: %w", err)
	}
	if err := s.replace(s.partSums(partition), sums); err != nil {
		return fmt.Errorf("eqmr file sink: commit partition %d: %w", partition, err)
	}
	if err := os.Rename(staged, shard); err != nil {
		return fmt.Errorf("eqmr file sink: commit partition %d: %w", partition, err)
	}
	return syncDir(filepath.Dir(shard))
}

// Finish writes an empty shard for each partition with no data, then the
// manifest.
func (s *fileSink) Finish(_ context.Context, run string, partitions int, empty []int) error {
	if err := s.claim(run); err != nil {
		return err
	}
	isEmpty := make(map[int]bool, len(empty))
	for _, p := range empty {
		isEmpty[p] = true
		// Nothing to write, so nothing can be seen half written.
		err := writeSynced(s.shard(p, partitions), nil)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("eqmr file sink: empty partition %d: %w", p, err)
		}
	}
	m := fileManifest{Run: run, Format: s.format.name, Empty: empty}
	for p := range partitions {
		entry := fileManifestEntry{Name: filepath.Base(s.shard(p, partitions)), fileSums: emptySums}
		if !isEmpty[p] {
			sums, err := s.committedSums(p, partitions)
			if err != nil {
				return err
			}
			entry.fileSums = sums
		}
		m.Files = append(m.Files, entry)
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("eqmr file sink: encode manifest: %w", err)
	}
	// Every call writes the same manifest, so the last may replace the first.
	if err := s.replace(s.base+".manifest", body); err != nil {
		return fmt.Errorf("eqmr file sink: manifest: %w", err)
	}
	return nil
}

// committedSums reads the sums of partition's committed shard.
func (s *fileSink) committedSums(partition, partitions int) (fileSums, error) {
	var sums fileSums
	ok, err := exists(s.shard(partition, partitions))
	if err != nil {
		return sums, fmt.Errorf("eqmr file sink: partition %d: %w", partition, err)
	}
	if !ok {
		return sums, worker.MoveErrorf("eqmr file sink: partition %d has no committed output", partition)
	}
	raw, err := os.ReadFile(s.partSums(partition))
	if err != nil {
		return sums, fmt.Errorf("eqmr file sink: partition %d sums: %w", partition, err)
	}
	if err := json.Unmarshal(raw, &sums); err != nil {
		return sums, worker.MoveErrorf("eqmr file sink: partition %d sums: %v", partition, err)
	}
	return sums, nil
}

// replace writes body to path whole: synced aside, then renamed over it.
func (s *fileSink) replace(path string, body []byte) error {
	if err := os.MkdirAll(s.staged, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(s.staged, filepath.Base(path)+".tmp-"+entroq.GenHex16())
	if err := writeSynced(tmp, body); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return syncDir(filepath.Dir(path))
}

// Sweep removes the staging directory of output run owns. It claims nothing: a
// run that never staged has nothing here.
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

// fileSinkWriter writes one staged attempt, summing what it writes.
type fileSinkWriter struct {
	sink *fileSink
	ref  fileStaged
	f    *os.File
	hash hash.Hash
	w    *bufio.Writer
}

func (w *fileSinkWriter) Write(_ context.Context, key, value string) error {
	line, err := w.sink.format.line(key, value)
	if err != nil {
		return err
	}
	line += "\n"
	if _, err := w.w.WriteString(line); err != nil {
		return fmt.Errorf("eqmr file sink: write: %w", err)
	}
	w.ref.Records++
	w.ref.Bytes += int64(len(line))
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
	w.ref.SHA256 = hex.EncodeToString(w.hash.Sum(nil))
	return json.Marshal(w.ref)
}

// exists reports whether path exists.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// writeSynced writes body to a new file at path and syncs it, or fails with
// os.ErrExist if path exists.
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

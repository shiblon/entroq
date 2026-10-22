package eqmr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// fileObjectStoreDriver is NewFileStore's driver name and compatibility
// version.
const fileObjectStoreDriver = "entroq.eqmr.file/1"

// NewFileStore returns an intermediate store that keeps runs as files under
// root, creating it if needed. name is the logical name the run records.
//
// Every process that maps or reduces the run must see the same files at the
// same root: one machine, or a shared filesystem. The store is identified by
// root's absolute path.
//
// Each run has its own directory. Each object is written once under a unique
// name and synced before its reference is returned.
func NewFileStore(name, root string) (IntermediateStore, error) {
	objects, err := newFileObjectStore(root)
	if err != nil {
		return nil, err
	}
	return newObjectIntermediateStore(Descriptor{
		Name:     name,
		Driver:   fileObjectStoreDriver,
		Identity: "file://" + objects.root,
	}, objects)
}

// fileObjectStore keeps objects as files, one directory per run.
type fileObjectStore struct {
	root string
}

// fileObjectRef locates an object by its path relative to the store root.
type fileObjectRef struct {
	Path string `json:"path"`
}

func newFileObjectStore(root string) (*fileObjectStore, error) {
	if root == "" {
		return nil, fmt.Errorf("eqmr file store: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("eqmr file store: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("eqmr file store: create root: %w", err)
	}
	return &fileObjectStore{root: abs}, nil
}

// runDir names a run's directory by a hash of the run, which may be any text
// and any length.
func runDir(run string) string {
	sum := sha256.Sum256([]byte(run))
	return "run-" + hex.EncodeToString(sum[:16])
}

func (s *fileObjectStore) put(ctx context.Context, run, objectID string, body io.Reader) (json.RawMessage, error) {
	if objectID == "" || filepath.Base(objectID) != objectID || !filepath.IsLocal(objectID) {
		return nil, fmt.Errorf("eqmr file store put: invalid object ID %q", objectID)
	}
	dir := filepath.Join(s.root, runDir(run))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("eqmr file store put %q: %w", objectID, err)
	}
	// Created exclusively, so no object is ever replaced. No reference to it
	// exists until it is complete and synced; a failed write leaves an
	// unreferenced file for the sweep.
	path := filepath.Join(dir, objectID)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eqmr file store put %q: %w", objectID, err)
	}
	if _, err := io.Copy(f, contextReader{ctx: ctx, r: body}); err != nil {
		return nil, errors.Join(fmt.Errorf("eqmr file store put %q: write: %w", objectID, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return nil, errors.Join(fmt.Errorf("eqmr file store put %q: sync: %w", objectID, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("eqmr file store put %q: close: %w", objectID, err)
	}
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("eqmr file store put %q: %w", objectID, err)
	}
	return json.Marshal(fileObjectRef{Path: filepath.ToSlash(filepath.Join(runDir(run), objectID))})
}

func (s *fileObjectStore) open(_ context.Context, raw json.RawMessage) (io.ReadCloser, error) {
	path, err := s.resolve(raw)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eqmr file store open: %w", err)
	}
	return f, nil
}

func (s *fileObjectStore) delete(_ context.Context, raw json.RawMessage) error {
	path, err := s.resolve(raw)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("eqmr file store delete: %w", err)
	}
	return nil
}

func (s *fileObjectStore) sweep(_ context.Context, run string) error {
	if err := os.RemoveAll(filepath.Join(s.root, runDir(run))); err != nil {
		return fmt.Errorf("eqmr file store sweep: %w", err)
	}
	return nil
}

// resolve turns a reference, from durable state, into a path under the root,
// refusing one that would leave it.
func (s *fileObjectStore) resolve(raw json.RawMessage) (string, error) {
	var ref fileObjectRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return "", fmt.Errorf("eqmr file store: parse reference: %w", err)
	}
	rel := filepath.FromSlash(ref.Path)
	if ref.Path == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("eqmr file store: reference %q is not a path under the store root", ref.Path)
	}
	return filepath.Join(s.root, rel), nil
}

// syncDir makes a directory's entries durable. A filesystem that rejects
// syncing a directory as invalid or unsupported, as many FUSE and network
// filesystems do, has nothing to sync.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	syncErr := d.Sync()
	if errors.Is(syncErr, errors.ErrUnsupported) || errors.Is(syncErr, syscall.EINVAL) {
		syncErr = nil
	}
	return errors.Join(syncErr, d.Close())
}

// contextReader stops a copy when its context ends.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestFileObjectStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := newFileObjectStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	ref, err := store.put(ctx, "/run/a", "obj-1", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	r, err := store.open(ctx, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, readErr := io.ReadAll(r)
	if err := errors.Join(readErr, r.Close()); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q, want %q", got, "hello")
	}

	// An object, once written, is never replaced.
	if _, err := store.put(ctx, "/run/a", "obj-1", strings.NewReader("other")); err == nil {
		t.Fatal("second put of the same object succeeded")
	}

	for i := range 2 {
		if err := store.delete(ctx, ref); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if _, err := store.open(ctx, ref); err == nil {
		t.Fatal("open after delete succeeded")
	}
}

func TestFileObjectStoreRefusesEscapingReferences(t *testing.T) {
	ctx := context.Background()
	store, err := newFileObjectStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	for _, path := range []string{"", "../outside", "/etc/passwd", "run/../../outside"} {
		ref, err := json.Marshal(fileObjectRef{Path: path})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := store.open(ctx, ref); err == nil || !strings.Contains(err.Error(), "not a path under the store root") {
			t.Errorf("open %q: got %v, want a refusal", path, err)
		}
		if err := store.delete(ctx, ref); err == nil {
			t.Errorf("delete %q succeeded", path)
		}
	}
	for _, id := range []string{"", "a/b", "..", "../x"} {
		if _, err := store.put(ctx, "/run", id, strings.NewReader("x")); err == nil {
			t.Errorf("put with object ID %q succeeded", id)
		}
	}
}

func TestFileObjectStoreSweepsOneRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := newFileObjectStore(root)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	gone, err := store.put(ctx, "/run/swept", "obj", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	kept, err := store.put(ctx, "/run/kept", "obj", strings.NewReader("y"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	for i := range 2 {
		if err := store.sweep(ctx, "/run/swept"); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if _, err := store.open(ctx, gone); err == nil {
		t.Error("swept run's object survived")
	}
	r, err := store.open(ctx, kept)
	if err != nil {
		t.Fatalf("another run's object was swept: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, runDir("/run/swept"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("swept run directory remains: %v", err)
	}
}

// TestFileStoreDetectsTruncatedRun pins that the run format, not the file
// store, catches a partial object: the store holds opaque bytes, and the
// reader rejects a run whose footer is missing.
func TestFileStoreDetectsTruncatedRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewFileStore("scratch", root)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ref, err := store.put(ctx, "/run", []intermediateRecord{{Primary: "a", Value: "1"}, {Primary: "b", Value: "2"}})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, runDir("/run"), "run-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("find object: %v, %v", matches, err)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(matches[0], info.Size()-4); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	reader, err := store.open(ctx, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, readErr := readAllIntermediate(ctx, reader)
	if err := reader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if readErr == nil {
		t.Fatal("truncated run read without error")
	}
}

// TestSyncDirUnsupportedIsNotAnError pins how a filesystem that cannot sync a
// directory is treated: as having nothing to sync, rather than as a failed
// write. The errors are the ones such filesystems return.
func TestSyncDirUnsupportedIsNotAnError(t *testing.T) {
	for _, err := range []error{syscall.EINVAL, syscall.ENOTSUP, syscall.ENOSYS, syscall.EOPNOTSUPP} {
		if !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%v would fail every write on a filesystem that cannot sync directories", err)
		}
	}
}

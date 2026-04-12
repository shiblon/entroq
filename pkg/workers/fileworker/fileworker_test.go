package fileworker

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func TestWorker_run(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to create eq: %v", err)
	}
	defer eq.Close()

	tempDir, err := ioutil.TempDir("", "fileworker-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	queue := "/test/files"
	content := "hello world"

	// Insert a task.
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue(content))); err != nil {
		t.Fatalf("Failed to insert task: %v", err)
	}

	// Run worker in background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, eq,
			WithDir(tempDir),
			WithPrefix("test-"),
			Watching(queue),
		)
	}()

	// Wait for file to appear.
	var files []os.FileInfo
	start := time.Now()
	for time.Since(start) < 2*time.Second {
		files, _ = ioutil.ReadDir(tempDir)
		if len(files) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(files) == 0 {
		t.Fatal("Timed out waiting for file to appear")
	}

	found := false
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "test-") && strings.HasSuffix(f.Name(), ".json") {
			found = true
			path := filepath.Join(tempDir, f.Name())
			data, err := ioutil.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read created file: %v", err)
			}
			// Value is JSON encoded string "hello world" -> "\"hello world\""
			expected := "\"" + content + "\""
			if string(data) != expected {
				t.Errorf("Content mismatch: expected %q, got %q", expected, string(data))
			}
		}
	}

	if !found {
		t.Errorf("No matching file found in %v", tempDir)
	}

	cancel()
	err = <-errCh
	if err != nil && !entroq.IsCanceled(err) {
		t.Errorf("Worker failed: %v", err)
	}
}

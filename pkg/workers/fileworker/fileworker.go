package fileworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// Worker provides logic for writing task values to files.
type Worker struct {
}

// Option defines configuration for the file worker.
type Option func(*Worker, *[]worker.RunOption, *runOpt)

type runOpt struct {
	dir    string
	prefix string
}

// WithDir sets the output directory for files.
func WithDir(dir string) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		ro.dir = dir
	}
}

// WithPrefix sets the prefix for filenames.
func WithPrefix(prefix string) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		ro.prefix = prefix
	}
}

// WithRunOption allows passing core worker.RunOption directly.
func WithRunOption(opt worker.RunOption) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		*wo = append(*wo, opt)
	}
}

// Watching specifies the queues to watch.
func Watching(qs ...string) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		*wo = append(*wo, worker.Watching(qs...))
	}
}

// WithLease sets the lease duration.
func WithLease(d time.Duration) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		*wo = append(*wo, worker.WithLease(d))
	}
}

// New creates a new file worker.
func New() *Worker {
	return &Worker{}
}

// Run starts the file worker.
func (fw *Worker) Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	ro := &runOpt{
		prefix: "task-",
	}
	var workerRunOpts []worker.RunOption

	for _, opt := range opts {
		opt(fw, &workerRunOpts, ro)
	}

	if ro.dir == "" {
		return fmt.Errorf("fileworker: output directory must be specified")
	}

	if err := os.MkdirAll(ro.dir, 0775); err != nil {
		return fmt.Errorf("fileworker: failed to create directory: %w", err)
	}

	handler := func(ctx context.Context, task *entroq.Task, value json.RawMessage) ([]entroq.ModifyArg, error) {
		// Create a unique filename with timestamp to handle potential crashes/retries unambiguously.
		ts := strings.ReplaceAll(time.Now().UTC().Format("20060102-150405.000"), ".", "")
		fname := fmt.Sprintf("%s%s-%s.json", ro.prefix, task.ID, ts)
		target := filepath.Join(ro.dir, fname)

		tmpName := filepath.Join(ro.dir, ".tmp."+fname)

		// Write to temp file.
		if err := os.WriteFile(tmpName, value, 0664); err != nil {
			return nil, fmt.Errorf("fileworker: write temp %q: %w", tmpName, err)
		}

		// Atomic Rename.
		if err := os.Rename(tmpName, target); err != nil {
			os.Remove(tmpName) // Cleanup temp on failure.
			return nil, fmt.Errorf("fileworker: rename to %q: %w", target, err)
		}

		// Return delete arg.
		return []entroq.ModifyArg{task.Delete()}, nil
	}

	return worker.New(eq, worker.WithDoModify(handler)).Run(ctx, workerRunOpts...)
}

// Run creates and runs a file worker in a single call.
func Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	return New().Run(ctx, eq, opts...)
}

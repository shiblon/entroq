package appendworker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// Appender is anything that can accept a byte slice and append it to a log.
// This is typically satisfied by a stuffedio/wal.WAL.
type Appender interface {
	Append(b []byte) error
}

// Worker provides logic for appending task values to a journal.
type Worker struct {
	appender Appender

	runOpts []worker.RunOption
	qs      []string
}

// Option defines configuration for the append worker.
type Option func(*Worker)

// WithRunOption allows passing core worker.RunOption directly.
func WithRunOption(opt worker.RunOption) Option {
	return func(w *Worker) {
		w.runOpts = append(w.runOpts, opt)
	}
}

// Watching specifies the queues to watch.
func Watching(qs ...string) Option {
	return func(w *Worker) {
		w.runOpts = append(w.runOpts, worker.Watching(qs...))
	}
}

// New creates a new append worker that uses the provided appender.
func New(a Appender) *Worker {
	return &Worker{
		appender: a,
	}
}

// Run starts the append worker.
func (aw *Worker) Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	for _, opt := range opts {
		opt(aw)
	}

	if aw.appender == nil {
		return fmt.Errorf("appender is nil")
	}

	handler := func(ctx context.Context, task *entroq.Task, _ json.RawMessage) ([]entroq.ModifyArg, error) {
		if err := aw.appender.Append(task.Value); err != nil {
			return nil, fmt.Errorf("append task %s: %w", task.ID, err)
		}
		return []entroq.ModifyArg{task.Delete()}, nil
	}

	return worker.New(eq, worker.WithDoModify(handler)).Run(ctx, aw.runOpts...)
}

// Run creates and runs an append worker in a single call.
func Run(ctx context.Context, eq *entroq.EntroQ, a Appender, opts ...Option) error {
	return New(a).Run(ctx, eq, opts...)
}

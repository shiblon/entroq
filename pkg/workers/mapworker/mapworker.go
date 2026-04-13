package mapworker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// Input[T] is the task value for a Worker.
type Input[T any] struct {
	Val    T      `json:"val"`
	Outbox string `json:"outbox"`
	Errbox string `json:"errbox"`
}

// Output[U] is the result of a Map task.
type Output[U any] struct {
	Val U      `json:"val"`
	Err string `json:"err"`
}

// Worker[T, U] provides the transformation logic.
type Worker[T, U any] struct {
	f func(context.Context, T) (U, error)
}

// Option[T, U] defines options that can be passed to Run.
type Option[T, U any] func(*Worker[T, U], *[]worker.Option[Input[T]], *[]worker.RunOption)

// WithQueues sets the queues the map worker should claim from.
func WithQueues[T, U any](qs ...string) Option[T, U] {
	return func(mw *Worker[T, U], wo *[]worker.Option[Input[T]], ro *[]worker.RunOption) {
		*ro = append(*ro, worker.Watching(qs...))
	}
}

// WithLease sets the lease duration for claimed tasks.
func WithLease[T, U any](d time.Duration) Option[T, U] {
	return func(mw *Worker[T, U], wo *[]worker.Option[Input[T]], ro *[]worker.RunOption) {
		*ro = append(*ro, worker.WithLease(d))
	}
}

// WithWorkerOption allows passing core worker options directly.
func WithWorkerOption[T, U any](opt worker.Option[Input[T]]) Option[T, U] {
	return func(mw *Worker[T, U], wo *[]worker.Option[Input[T]], ro *[]worker.RunOption) {
		*wo = append(*wo, opt)
	}
}

// WithRunOption allows passing core run options directly.
func WithRunOption[T, U any](opt worker.RunOption) Option[T, U] {
	return func(mw *Worker[T, U], wo *[]worker.Option[Input[T]], ro *[]worker.RunOption) {
		*ro = append(*ro, opt)
	}
}

// doWork is the internal handler that matches worker.DoModifyRun.
func (mw *Worker[T, U]) doWork(ctx context.Context, t *entroq.Task, input Input[T], _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
	outbox := input.Outbox
	if outbox == "" {
		outbox = t.Queue + "/done"
	}

	errbox := input.Errbox
	if errbox == "" {
		errbox = t.Queue + "/error"
	}

	res, err := mw.f(ctx, input.Val)
	if err != nil {
		errOutput := Output[U]{Err: err.Error()}
		errBytes, mErr := json.Marshal(errOutput)
		if mErr != nil {
			return nil, fmt.Errorf("marshal map error output: %w", mErr)
		}
		return []entroq.ModifyArg{
			t.Delete(),
			entroq.InsertingInto(errbox, entroq.WithRawValue(errBytes)),
		}, nil
	}

	output := Output[U]{Val: res}
	outBytes, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("marshal map output: %w", err)
	}

	return []entroq.ModifyArg{
		t.Delete(),
		entroq.InsertingInto(outbox, entroq.WithRawValue(outBytes)),
	}, nil
}

// New creates a new MapWorker ready to be configured.
func New[T any, U any](f func(context.Context, T) (U, error), opts ...Option[T, U]) *Worker[T, U] {
	mw := &Worker[T, U]{f: f}
	return mw
}

// Run creates and runs a MapWorker in a single one-shot call.
func Run[T any, U any](ctx context.Context, eq *entroq.EntroQ, f func(context.Context, T) (U, error), opts ...any) error {
	var mapOpts []Option[T, U]
	var runOpts []worker.RunOption

	for _, opt := range opts {
		switch o := opt.(type) {
		case Option[T, U]:
			mapOpts = append(mapOpts, o)
		case func(*Worker[T, U], *[]worker.Option[Input[T]], *[]worker.RunOption):
			mapOpts = append(mapOpts, o)
		case worker.RunOption:
			runOpts = append(runOpts, o)
		default:
			return fmt.Errorf("mapworker: unknown option type %T", opt)
		}
	}

	mw := New(f, mapOpts...)
	var workerOpts []worker.Option[Input[T]]

	// Default: use the struct's doWork method.
	workerOpts = append(workerOpts, worker.WithDoModify(mw.doWork))

	for _, opt := range mapOpts {
		opt(mw, &workerOpts, &runOpts)
	}

	return worker.New(eq, workerOpts...).Run(ctx, runOpts...)
}

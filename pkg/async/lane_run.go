package async

import (
	"context"
	"errors"
	"fmt"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"go.opentelemetry.io/otel/metric"
)

// turn is what a committed lane turn means for the worker claiming on the
// lane.
type turn int

const (
	// turnNext keeps claiming on the same queue.
	turnNext turn = iota
	// turnRotated moved this side's queue: claim on the new one.
	turnRotated
	// turnDone finished this lane's direction.
	turnDone
)

// errLaneExpired is a lane whose queue reached its GC deadline unrotated.
var errLaneExpired = errors.New("eqlink lane queue expired")

// laneTurns records the turn a committed result ended with. A handler builds
// each result with end, so the turn is recorded only once its commit lands.
type laneTurns struct {
	last turn
}

// end makes r a turn of kind t: onSuccess (which may be nil) runs once r
// commits, and a rotation or a finish stops the worker after it.
func (l *laneTurns) end(r *worker.Result, t turn, onSuccess func(context.Context) error) *worker.Result {
	r = r.OnSuccess(func(ctx context.Context) error {
		l.last = t
		if onSuccess == nil {
			return nil
		}
		return onSuccess(ctx)
	})
	if t != turnNext {
		r = r.ThenStop()
	}
	return r
}

// noDocs is the take phase of a lane that holds no docs.
func noDocs[T any](context.Context, entroq.Reader, *worker.Work[T]) (*worker.TakeResult, error) {
	return nil, nil
}

// runLane claims on lanes' local queue until a turn finishes the lane's
// direction, starting a new worker on the new queue after each rotation. done
// reports that a turn finished it; once ctx ends it returns neither done nor an
// error, and errLaneExpired if the queue reaches its GC deadline first.
//
// handle makes the lane's handler, which builds each result with the
// laneTurns it is given.
func runLane[T any](ctx context.Context, eq *entroq.EntroQ, mp metric.MeterProvider, claimant string, lanes *sessionLanes,
	take worker.TakeRun[T], handle func(*laneTurns) worker.DoModifyRun[T], opts ...worker.RunOption) (done bool, err error) {
	for {
		var turns laneTurns
		queue := lanes.local.queue
		wctx, stop := context.WithDeadline(ctx, lanes.local.collectAt)
		err = worker.New(eq,
			worker.WithTakeDocs(take),
			worker.WithDoModify(handle(&turns)),
			worker.WithMeterProvider[T](mp),
		).Run(wctx, append(opts, worker.Watching(queue), worker.AsClaimant(claimant))...)
		expired := errors.Is(wctx.Err(), context.DeadlineExceeded)
		stop()
		switch {
		case err != nil:
			return false, err
		case turns.last == turnRotated:
			continue
		case turns.last == turnDone:
			return true, nil
		case ctx.Err() != nil:
			return false, nil
		case expired:
			return false, errLaneExpired
		}
		return false, fmt.Errorf("lane worker on %q stopped without a reason", queue)
	}
}

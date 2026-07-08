// Package pullworker provides a worker that pulls tasks from a queue on one
// EntroQ instance and delivers them, exactly once in effect, into a queue on
// another instance.
//
// # Why "pull"
//
// EntroQ has no push: every task acquisition is a Claim. This worker runs next
// to the destination instance, reaches up to the source instance, and claims
// tasks out of it -- so from the operator's vantage it pulls work down from
// upstream, the same intuition as "git pull". Running it beside the destination
// also keeps the delivery work local; only the claim from the source crosses the
// wire.
//
// # Exactly-once delivery
//
// The hazard with moving a task between two independent instances is the gap
// between "delivered to the destination" and "deleted from the source": a crash
// in that gap re-delivers, and if the destination already consumed the task the
// duplicate is invisible. We close it with a dedup tombstone.
//
// For each claimed source task the worker performs ONE atomic Modify on the
// destination: insert a fresh task into the inbox AND insert a value-stripped
// tombstone keyed by a deterministic transfer ID derived from the source task.
// Only then is the source task deleted. Because the tombstone is written in the
// same Modify as the inbox task, it exists independently of the inbox task and
// outlives its consumption. A crash before the source delete leads to a re-claim
// and a re-attempt; the tombstone insert collides, which aborts the whole Modify
// (so no second inbox task is created), and the worker proceeds to delete the
// source. Convergence is to exactly one inbox task.
//
// The transfer ID must be a deterministic, collision-resistant function of the
// source task's identity so that any re-attempt recomputes the same value
// without persisting anything. It must fit EntroQ's 64-character id limit, so it
// is a hash rather than a prefix of the (already up-to-64-char) source id.
//
// This worker does no work in the work phase: moving tasks between queues is the
// whole job, so the queue topology is the state machine. Everything happens in
// finalization, where the source task has a stable (renewed) version safe to
// delete. Deliver, delete source, clean up tombstone -- in that order -- as one
// straight-line sequence.
//
// # Tombstone lifetime
//
// A tombstone is only needed while a re-delivery is still possible, i.e. while
// the source task still exists. Once the source is deleted that window is
// closed, so the worker deletes its own tombstone immediately after deleting the
// source -- it owns the tombstone it just inserted (its claimant matches), so no
// special privilege is required. Tombstones therefore linger only when a crash
// interrupts the worker between delivery and that cleanup.
//
// Those orphans are reaped by arrival time: each tombstone is inserted At = now
// + TTL, and the destination server's built-in garbage collector removes any
// whose time has come -- the tombstone queue ("graveyard") carries a /gc=
// marker (see Graveyard), so no separate reaper is needed as long as that
// server runs GC, which is the default. The TTL is the dedup-retention window
// and the single safety knob: a duplicate is only possible if a tombstone is
// reaped while a crashed pull can still re-attempt -- i.e. only if recovery
// takes longer than the TTL. Size it well above worst-case recovery. GC is the
// safety net for crash orphans, not the primary cleanup path.
package pullworker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// DefaultTTL is the tombstone retention window used when WithTTL is not set.
// It must exceed the worst-case time to recover and finish a delivery after a
// crash; see the package doc.
const DefaultTTL = time.Hour

// pullHandler is the handler for the pull worker, specified using the full
// approach because it requires variable sharing between work and finish
// functions.
type pullHandler struct {
	eq     *entroq.EntroQ
	worker *Worker

	tombstone *entroq.Task
}

func (h *pullHandler) TakeDocs(context.Context, *entroq.Task, json.RawMessage) ([]*entroq.DocClaim, error) {
	return nil, nil
}

func (h *pullHandler) DoWork(ctx context.Context, task *entroq.Task, value json.RawMessage, docs []*entroq.Doc) error {
	tombstoneID := h.worker.transferID(task)
	resp, err := h.worker.dst.Modify(ctx,
		entroq.InsertingInto(h.worker.graveyard,
			entroq.WithID(tombstoneID),
			entroq.WithArrivalTimeIn(h.worker.ttl)),
		entroq.InsertingInto(h.worker.inbox, entroq.WithRawValue(value)),
	)
	if err != nil {
		if de, isDep := entroq.AsDependency(err); isDep && de.HasCollisions() {
			log.Printf("task %v already delivered to remote, cleaning up", task.IDVersion())
			return nil
		}
		return fmt.Errorf("delivery to remote %q: %v: %w", h.worker.inbox, err, worker.RetryError)
	}
	h.tombstone = resp.InsertedTasks[0]
	return nil
}

func (h *pullHandler) Finish(ctx context.Context, task *entroq.Task, value json.RawMessage, docs []*entroq.Doc) error {
	if _, err := h.eq.Modify(ctx, task.Delete()); err != nil {
		return fmt.Errorf("pull worker finish: %w", err)
	}
	if h.tombstone != nil {
		if _, err := h.worker.dst.Modify(ctx, h.tombstone.Delete()); err != nil {
			log.Printf("pull: tombstone cleanup %v (left for destination gc): %v", h.tombstone.IDVersion(), err)
		}
	}
	return nil
}

// Worker pulls tasks from a source instance (the one it claims from) into an
// inbox on a destination instance, delivering each exactly once in effect via a
// dedup tombstone. See the package doc for the protocol.
type Worker struct {
	dst       *entroq.EntroQ
	inbox     string
	graveyard string // queue for tombstones
	ttl       time.Duration
	source    string
}

// Option configures a pull Worker and the underlying worker.Worker. Following
// the convention of the other standard workers, an option may set Worker fields
// and/or append core worker and run options.
type Option func(*Worker, *[]worker.Option[json.RawMessage], *[]worker.RunOption)

// WithDest sets the destination EntroQ client -- the instance tasks are
// delivered into. Required.
func WithDest(dst *entroq.EntroQ) Option {
	return func(w *Worker, _ *[]worker.Option[json.RawMessage], _ *[]worker.RunOption) {
		w.dst = dst
	}
}

// WithInbox sets the destination inbox queue that delivered tasks land in.
// Required. The graveyard queue defaults to <inbox>/_graveyard unless overridden
// with WithGraveyard.
func WithInbox(q string) Option {
	return func(w *Worker, _ *[]worker.Option[json.RawMessage], _ *[]worker.RunOption) {
		w.inbox = q
	}
}

// WithGraveyard overrides the graveyard queue (default from DefaultGraveyard).
// An override should include a /gc= component (as is true for DefaultGraveyard) so the
// destination server's GC reaps its orphans; otherwise point your own reaper at it.
func WithTomb(q string) Option {
	return func(w *Worker, _ *[]worker.Option[json.RawMessage], _ *[]worker.RunOption) {
		w.graveyard = q
	}
}

// WithTTL sets the tombstone retention window (default DefaultTTL). It is the
// dedup safety knob and must exceed worst-case recovery time.
func WithTTL(d time.Duration) Option {
	return func(w *Worker, _ *[]worker.Option[json.RawMessage], _ *[]worker.RunOption) {
		w.ttl = d
	}
}

// WithSource sets a stable identifier for the source instance, mixed into the
// transfer ID so that two sources with a coincidentally equal task id cannot
// collide on the destination. Use a stable name (not a per-process value); it
// must be identical across restarts for re-attempts to dedup.
func WithSource(name string) Option {
	return func(w *Worker, _ *[]worker.Option[json.RawMessage], _ *[]worker.RunOption) {
		w.source = name
	}
}

// WithQueues sets the source queues to claim tasks from.
func WithQueues(qs ...string) Option {
	return func(_ *Worker, _ *[]worker.Option[json.RawMessage], ro *[]worker.RunOption) {
		*ro = append(*ro, worker.Watching(qs...))
	}
}

// WithLease sets the lease duration for claimed source tasks.
func WithLease(d time.Duration) Option {
	return func(_ *Worker, _ *[]worker.Option[json.RawMessage], ro *[]worker.RunOption) {
		*ro = append(*ro, worker.WithLease(d))
	}
}

// WithWorkerOption passes a core worker option through.
func WithWorkerOption(opt worker.Option[json.RawMessage]) Option {
	return func(_ *Worker, wo *[]worker.Option[json.RawMessage], _ *[]worker.RunOption) {
		*wo = append(*wo, opt)
	}
}

// WithRunOption passes a core run option through.
func WithRunOption(opt worker.RunOption) Option {
	return func(_ *Worker, _ *[]worker.Option[json.RawMessage], ro *[]worker.RunOption) {
		*ro = append(*ro, opt)
	}
}

// transferID derives the deterministic, collision-resistant dedup key for a
// source task. It mixes the source name and the source task id and encodes the
// hash to stay within EntroQ's 64-character id limit.
func (w *Worker) transferID(t *entroq.Task) string {
	sum := sha256.Sum256([]byte(w.source + "\x00" + t.ID))
	return "xfer-" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// DefaultGraveyard returns the default graveyard queue for an inbox. Its name
// carries a gc=0 component so the destination server's built-in garbage
// collector reaps crash orphans once their TTL (arrival time) elapses; no
// separate reaper is needed as long as that server runs GC, which is the default.
func DefaultGraveyard(inbox string) string {
	return path.Join(inbox, "_graveyard", "gc=0")
}

// New creates a pull Worker ready to be configured by Run.
func New() *Worker {
	return &Worker{ttl: DefaultTTL}
}

// Run creates and runs a pull worker in a single call. src is the source client
// (the instance tasks are claimed from); WithDest supplies the destination
// client. Blocks until ctx is canceled or an unrecoverable error occurs.
//
// Run delivers and cleans up its own tombstones on the happy path; crash orphans
// are reaped by the destination server's built-in GC, since the graveyard queue
// (see DefaultGraveyard) carries a /gc= marker.
func Run(ctx context.Context, src *entroq.EntroQ, opts ...Option) error {
	w := New()
	var workerOpts []worker.Option[json.RawMessage]
	var runOpts []worker.RunOption
	for _, opt := range opts {
		opt(w, &workerOpts, &runOpts)
	}

	if w.dst == nil {
		return fmt.Errorf("pull worker: destination client required (WithDest)")
	}
	if w.inbox == "" {
		return fmt.Errorf("pull worker: inbox queue required (WithInbox)")
	}
	if w.graveyard == "" {
		w.graveyard = DefaultGraveyard(w.inbox)
	}
	if w.ttl <= 0 {
		return fmt.Errorf("pull worker: tombstone TTL must be positive")
	}

	return worker.New(src,
		worker.WithMakeHandler(func() (worker.Handler[json.RawMessage], error) {
			return &pullHandler{eq: src, worker: w}, nil
		}),
	).Run(ctx, runOpts...)
}

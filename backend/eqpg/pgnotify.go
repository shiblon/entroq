package eqpg

import (
	"context"
	"crypto/md5"
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/subq"
)

// PGNotifyWaiter implements entroq.NotifyWaiter using PostgreSQL LISTEN/NOTIFY.
//
// A single dedicated connection (via pq.Listener) handles all PG subscriptions.
// We can't rely on the connection pool for this because pg_notify only wakes
// listeners on the same connection.
//
// Postgres triggers fire NOTIFY automatically when tasks are inserted or
// updated, so the Go-side Notify method is a no-op. The embedded SubQ handles
// all Go-side wait/notify fanout; PGNotifyWaiter adds only the PG LISTEN
// lifecycle and the dispatch goroutine that bridges PG notifications into SubQ.
type PGNotifyWaiter struct {
	*subq.SubQ
	sync.Mutex

	listener *pq.Listener

	chRefs  map[string]int    // PG channel name → active Wait count
	chQueue map[string]string // PG channel name → queue name (for dispatch)

	cancel context.CancelFunc
}

// Compile-time interface check.
var _ entroq.NotifyWaiter = (*PGNotifyWaiter)(nil)

// NewPGNotifyWaiter creates a PGNotifyWaiter. connStr is used for the
// dedicated LISTEN connection (same connection string as the main pool).
func NewPGNotifyWaiter(ctx context.Context, connStr string) *PGNotifyWaiter {
	ctx, cancel := context.WithCancel(ctx)
	w := &PGNotifyWaiter{
		SubQ:    subq.New(),
		chRefs:  make(map[string]int),
		chQueue: make(map[string]string),
		cancel:  cancel,
	}
	w.listener = pq.NewListener(connStr,
		10*time.Second, // min reconnect interval
		time.Minute,    // max reconnect interval
		nil,            // event callback (nil = silent)
	)
	go w.dispatch(ctx)
	return w
}

// Close shuts down the waiter and its dedicated listener connection.
func (w *PGNotifyWaiter) Close() error {
	w.cancel()
	return w.listener.Close()
}

// dispatch is the background goroutine that reads PG notifications and wakes
// a waiter subscribed to the notified queue via SubQ.
func (w *PGNotifyWaiter) dispatch(ctx context.Context) {
	for {
		select {
		case n := <-w.listener.Notify:
			if n == nil {
				// nil means the listener reconnected; pq.Listener automatically
				// re-subscribes to all channels, so no action needed here.
				continue
			}
			if queue := w.queueForChannel(n.Channel); queue != "" {
				w.SubQ.Notify(queue)
			}
		case <-ctx.Done():
			return
		}
	}
}

// queueForChannel returns the queue name registered for the given PG channel,
// or empty string if none.
func (w *PGNotifyWaiter) queueForChannel(pgCh string) string {
	defer un(lock(w))
	return w.chQueue[pgCh]
}

// Notify is a no-op because the database trigger handles all notifications.
func (w *PGNotifyWaiter) Notify(string) {}

// subscribe sets up listeners on postgres channels for given EntroQ queue names.
// It maintains a mapping from more restrictive postgres channel names to queues,
// and maintains reference counts for graceful cleanup when unsubscribing.
func (w *PGNotifyWaiter) subscribe(queues []string) error {
	defer un(lock(w))
	for _, q := range queues {
		pgCh := pgChannelName(q)
		if w.chRefs[pgCh] == 0 {
			if err := w.listener.Listen(pgCh); err != nil {
				return fmt.Errorf("pg listen %q: %w", pgCh, err)
			}
			w.chQueue[pgCh] = q
		}
		w.chRefs[pgCh]++
	}
	return nil
}

// unsubscribe decrements ref counts and UNLISTENs any channel with no remaining waiters.
func (w *PGNotifyWaiter) unsubscribe(queues []string) {
	defer un(lock(w))
	for _, q := range queues {
		pgCh := pgChannelName(q)
		if w.chRefs[pgCh]--; w.chRefs[pgCh] == 0 {
			delete(w.chRefs, pgCh)
			delete(w.chQueue, pgCh)
			if err := w.listener.Unlisten(pgCh); err != nil {
				log.Printf("pg unlisten %q: %v", pgCh, err)
			}
		}
	}
}

// Wait subscribes to PG channels for the given queues, then delegates to SubQ
// to block until cond returns true, a notification arrives, the poll interval
// elapses, or the context is canceled.
func (w *PGNotifyWaiter) Wait(ctx context.Context, queues []string, poll time.Duration, cond func() bool) error {
	if err := w.subscribe(queues); err != nil {
		return fmt.Errorf("pg notify wait: %w", err)
	}
	defer w.unsubscribe(queues)
	return w.SubQ.Wait(ctx, queues, poll, cond)
}

func lock(l sync.Locker) func() {
	l.Lock()
	return l.Unlock
}

func un(f func()) {
	f()
}

var nonAlphanumericRE = regexp.MustCompile(`[^a-zA-Z0-9]`)

// pgChannelName returns the PostgreSQL notification channel name for a queue.
// This mirrors entroq_channel_name() in the schema — the two must stay in sync.
// Channel names are capped at 63 bytes (PostgreSQL identifier limit).
func pgChannelName(queue string) string {
	sanitized := nonAlphanumericRE.ReplaceAllString(queue, "_")
	if len(sanitized)+2 <= 63 {
		return "q_" + sanitized
	}
	hash := fmt.Sprintf("%x", md5.Sum([]byte(queue)))
	// q_(25)_(8-hex-hash)_(26) = 2+25+1+8+1+26 = 63 bytes.
	return "q_" + sanitized[:25] + "_" + hash[:8] + "_" + sanitized[len(sanitized)-26:]
}

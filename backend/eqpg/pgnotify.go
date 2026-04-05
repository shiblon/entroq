package eqpg

import (
	"context"
	"crypto/md5"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/subq"
	"golang.org/x/sync/errgroup"
)

// PGNotifyWaiter implements entroq.NotifyWaiter using PostgreSQL LISTEN/NOTIFY.
//
// A single dedicated connection (via pq.Listener) handles all PG subscriptions.
// If you are using a connection pool proxy, this won't work for you, you will
// just fall back to polling. If you are set up with Postgres as a backend for
// the gRPC service, you can just use the standard in-memory implementation
// anyway.
//
// If you are direct-connected to Postgres, then the stored procedures will
// fire NOTIFY during Modify events, and a heartbeat can cause it to trigger
// for tasks becoming available due to the passage of time.
//
// This is implemented in terms of SubQ, the normal in-memory implementation,
// which gracefully handles claim subscriptions, etc.
type PGNotifyWaiter struct {
	*subq.SubQ
	sync.Mutex

	listener *pq.Listener

	chRefs  map[string]int    // PG channel name -> active Wait count
	chQueue map[string]string // PG channel name -> queue name (for dispatch)

	cancel func() error
}

// Compile-time interface check.
var _ entroq.NotifyWaiter = (*PGNotifyWaiter)(nil)

// NewPGNotifyWaiter creates a PGNotifyWaiter. connStr is used for the
// dedicated LISTEN connection (same connection string as the main pool).
func NewPGNotifyWaiter(connStr string) *PGNotifyWaiter {
	w := &PGNotifyWaiter{
		SubQ:    subq.New(),
		chRefs:  make(map[string]int),
		chQueue: make(map[string]string),
	}
	w.listener = pq.NewListener(connStr,
		10*time.Second, // min reconnect interval
		time.Minute,    // max reconnect interval
		nil,            // event callback (nil = silent)
	)
	// The notification loop runs in a goroutine and its lifecycle is tied to
	// Close. Background context is correct, here.
	g, ctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	g.Go(func() error {
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
				return nil
			}
		}
	})
	w.cancel = func() error {
		cancel()
		return g.Wait()
	}
	return w
}

// Close shuts down the waiter and its dedicated listener connection.
func (w *PGNotifyWaiter) Close() error {
	var msgs []string
	if err := w.cancel(); err != nil {
		msgs = append(msgs, fmt.Sprintf("pg listener goroutine cancel: %v", err))
	}
	if err := w.listener.Close(); err != nil {
		msgs = append(msgs, fmt.Sprintf("pg listener conn close: %v", err))
	}
	if len(msgs) != 0 {
		return fmt.Errorf("close errors: $%v", strings.Join(msgs, " :: "))
	}
	return nil
}

// queueForChannel returns the queue name registered for the given PG channel,
// or empty string if none.
func (w *PGNotifyWaiter) queueForChannel(pgCh string) string {
	defer un(lock(w))
	return w.chQueue[pgCh]
}

// Notify provides immediate local intuition for workers sharing this gateway.
// Global workers are notified via the database's batch-broadcast notification.
func (w *PGNotifyWaiter) Notify(queue string) {
	w.SubQ.Notify(queue)
}

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
// This mirrors channel_name() in the schema -- the two must stay in sync.
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

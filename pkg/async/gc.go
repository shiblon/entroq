package async

import (
	"context"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/queues"
)

const (
	defaultGCInterval = 10 * time.Minute
	defaultGCGrace    = 15 * time.Second
)

// GCOption configures RunGCLoop and RunGC.
type GCOption func(*gcConfig)

type gcConfig struct {
	interval time.Duration
	grace    time.Duration
}

func newGCConfig(opts []GCOption) gcConfig {
	c := gcConfig{
		interval: defaultGCInterval,
		grace:    defaultGCGrace,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithGCInterval sets how often the GC scan runs. Defaults to 10 minutes.
func WithGCInterval(d time.Duration) GCOption {
	return func(c *gcConfig) {
		c.interval = d
	}
}

// WithGCGrace sets the extra time after a response queue's expiry timestamp
// before the GC considers it eligible for deletion. This prevents races at
// the timeout boundary where a sender may still be reading the response.
// Defaults to 15 seconds.
func WithGCGrace(d time.Duration) GCOption {
	return func(c *gcConfig) {
		c.grace = d
	}
}

// RunGCLoop runs RunGC periodically until ctx is canceled. Scan errors are
// logged and do not stop the loop.
func RunGCLoop(ctx context.Context, eq *entroq.EntroQ, root string, opts ...GCOption) error {
	c := newGCConfig(opts)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	log.Printf("GC started: root=%q interval=%v grace=%v", root, c.interval, c.grace)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := RunGC(ctx, eq, root, opts...); err != nil {
				log.Printf("gc scan error: %v", err)
			}
		}
	}
}

// RunGC performs one GC scan pass, deleting tasks from response queues whose
// expiry timestamp (plus grace) has passed. It uses TryClaim to drain each
// queue, which provides mutual exclusion between concurrent GC instances.
func RunGC(ctx context.Context, eq *entroq.EntroQ, root string, opts ...GCOption) error {
	c := newGCConfig(opts)
	now := time.Now()

	queueMap, err := eq.Queues(ctx, entroq.MatchPrefix(path.Join(root, "/")))
	if err != nil {
		return fmt.Errorf("list queues: %w", err)
	}

	var cleaned, skipped int
	for qname := range queueMap {
		if !strings.Contains(qname, "/response/") {
			continue
		}
		params := queues.PathParams(qname)
		expVals, ok := params["exp"]
		if !ok {
			continue
		}
		expUnix, err := strconv.ParseInt(expVals[0], 10, 64)
		if err != nil {
			continue
		}
		if now.Before(time.Unix(expUnix, 0).Add(c.grace)) {
			skipped++
			continue
		}
		success := true
		for {
			task, err := eq.TryClaim(ctx, entroq.From(qname))
			if err != nil {
				log.Printf("gc claim %s: %v", qname, err)
				success = false
				break
			}
			if task == nil {
				break
			}
			if _, err := eq.Modify(ctx, task.Delete()); err != nil {
				log.Printf("gc delete task in %s: %v", qname, err)
				success = false
			}
		}
		if success {
			cleaned++
		}
	}

	if cleaned > 0 || skipped > 0 {
		log.Printf("gc scan: %d response queues cleaned, %d within grace window", cleaned, skipped)
	}

	return nil
}

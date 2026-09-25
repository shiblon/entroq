package eqredis

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff bounds for retrying a transaction that lost a race.
const (
	minRetryBackoff = 100 * time.Microsecond
	maxRetryBackoff = 20 * time.Millisecond
)

// waitToRetry waits before attempt n+1 of a transaction whose WATCH fired: a
// random time up to an exponentially growing ceiling, so writers contending
// for the same keys spread out instead of colliding again at once.
func waitToRetry(ctx context.Context, n int) error {
	ceiling := min(minRetryBackoff<<min(n, 16), maxRetryBackoff)
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(ceiling)))):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

package worker

import (
	"math/rand/v2"
	"time"
)

// jitterSpread returns a random duration in [0, spread], or zero when spread
// is not positive. It is the shared arithmetic behind jitterLater and
// jitterEarlier, which differ only in the sign they apply it with.
func jitterSpread(spread time.Duration) time.Duration {
	if spread <= 0 {
		return 0
	}
	return rand.N(spread + 1)
}

// jitterLater returns d lengthened by up to spread at random, so workers that
// computed the same d do not act at the same instant. Use it before a retry,
// where moving work later is always safe: tasks that lost the same race come
// back at different times instead of colliding again.
//
// Do not use it before a deadline. See jitterEarlier.
func jitterLater(d, spread time.Duration) time.Duration {
	return d + jitterSpread(spread)
}

// jitterEarlier returns d shortened by up to spread at random, never below
// zero. Use it for a wait that ends in a deadline which must not be missed,
// such as a lease renewal: shortening the wait only adds headroom, while
// jitterLater would eat into that headroom and can push a renewal past the
// lease's expiry, losing the claim.
//
// The two directions are not interchangeable. A renewal jitters earlier; a
// retry after contention jitters later.
func jitterEarlier(d, spread time.Duration) time.Duration {
	if j := jitterSpread(spread); j < d {
		return d - j
	}
	return 0
}

package worker

import (
	"math/rand/v2"
	"time"
)

// jitterSpread returns a random duration in [0, spread], or zero when spread
// is not positive.
func jitterSpread(spread time.Duration) time.Duration {
	if spread <= 0 {
		return 0
	}
	return rand.N(spread + 1)
}

// jitterLater returns d lengthened by up to spread at random, so workers that
// computed the same d do not act at the same instant. It is for a retry, where
// moving work later is always safe: tasks that lost the same race come back at
// different times instead of colliding again.
//
// A wait that ends in a deadline, such as a lease renewal, must not be
// lengthened this way -- it eats the headroom the deadline needs. Nothing here
// jitters a renewal; if something should, it has to move the wait EARLIER.
func jitterLater(d, spread time.Duration) time.Duration {
	return d + jitterSpread(spread)
}

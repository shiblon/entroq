package async

import (
	"context"
	"fmt"
	"time"
)

var defaultHeartbeatTiming = heartbeatTiming{
	after:   time.Minute,
	timeout: 3 * time.Minute,
}

type heartbeatTiming struct {
	after   time.Duration
	timeout time.Duration
}

func heartbeatTimingForTimeout(timeout time.Duration) heartbeatTiming {
	return heartbeatTiming{after: timeout / 3, timeout: timeout}
}

func (t heartbeatTiming) validate() error {
	if t.after <= 0 {
		return fmt.Errorf("heartbeat interval must be positive")
	}
	if t.timeout <= t.after {
		return fmt.Errorf("peer timeout must be after the heartbeat interval")
	}
	return nil
}

// peerLiveness tracks frames received from the remote EQLink. Sending does not
// prove peer health; only an incoming data or empty frame resets the deadline.
type peerLiveness struct {
	timing   heartbeatTiming
	activity chan struct{}
}

func newPeerLiveness(timing heartbeatTiming) *peerLiveness {
	return &peerLiveness{timing: timing, activity: make(chan struct{}, 1)}
}

func (l *peerLiveness) observed() {
	select {
	case l.activity <- struct{}{}:
	default:
	}
}

// run resets the peer deadline after every observed frame and calls timeout
// once when no frame arrives in time. Reset safely replaces either an active or
// expired deadline, so no separate stop-and-drain handshake is needed.
func (l *peerLiveness) run(ctx context.Context, timeout func()) error {
	timer := time.NewTimer(l.timing.timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			timeout()
			return nil
		case <-l.activity:
			timer.Reset(l.timing.timeout)
		case <-ctx.Done():
			return nil
		}
	}
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

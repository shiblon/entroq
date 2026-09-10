package async

import (
	"fmt"
	"path"
	"time"

	"github.com/shiblon/entroq/pkg/queues"
)

var defaultLaneTiming = laneTiming{
	lifetime:        30 * time.Minute,
	piggybackBefore: 10 * time.Minute,
	forceBefore:     5 * time.Minute,
}

type laneTiming struct {
	lifetime        time.Duration
	piggybackBefore time.Duration
	forceBefore     time.Duration
}

func (t laneTiming) validate() error {
	if t.lifetime <= 0 {
		return fmt.Errorf("queue lifetime must be positive")
	}
	if t.piggybackBefore <= t.forceBefore || t.piggybackBefore >= t.lifetime {
		return fmt.Errorf("piggyback threshold must be after the forced threshold and before queue expiry")
	}
	if t.forceBefore <= 0 {
		return fmt.Errorf("forced threshold must be positive")
	}
	return nil
}

// receiveLane is the queue on which one side awaits its next frame. Rotating
// it changes only the local inbox; the peer learns the new full queue name
// from the next outgoing frame.
type receiveLane struct {
	prefix    string
	session   string
	direction string
	queue     string
	collectAt time.Time
	timing    laneTiming
}

func newReceiveLane(prefix, session, direction string, now time.Time, timing laneTiming) *receiveLane {
	lane := &receiveLane{
		prefix:    prefix,
		session:   session,
		direction: direction,
		timing:    timing,
	}
	lane.rotate(now)
	return lane
}

func (l *receiveLane) rotate(now time.Time) string {
	collectAt := time.Unix(now.Add(l.timing.lifetime).Unix(), 0).UTC()
	if !collectAt.After(l.collectAt) {
		collectAt = l.collectAt.Add(time.Second)
	}
	l.collectAt = collectAt
	l.queue = sessionQueue(l.prefix, l.session, l.collectAt, l.direction)
	return l.queue
}

func (l *receiveLane) shouldPiggyback(now time.Time) bool {
	return !now.Before(l.collectAt.Add(-l.timing.piggybackBefore))
}

func (l *receiveLane) forceAt() time.Time {
	return l.collectAt.Add(-l.timing.forceBefore)
}

type peerLane struct {
	queue     string
	collectAt time.Time
}

type sessionLanes struct {
	local          *receiveLane
	peer           peerLane
	awaitingSwitch bool
}

// observePeer reports whether the next outgoing frame must reciprocate a peer
// switch. A changed peer queue instead completes the exchange when this side
// is already awaiting a reciprocal switch.
func (l *sessionLanes) observePeer(queue string) (reciprocate bool, err error) {
	changed, err := l.peer.observe(queue)
	if err != nil || !changed {
		return false, err
	}
	if l.awaitingSwitch {
		l.awaitingSwitch = false
		return false, nil
	}
	return true, nil
}

func (l *sessionLanes) initiateSwitch(now time.Time) string {
	l.awaitingSwitch = true
	return l.local.rotate(now)
}

func (l *sessionLanes) reciprocateSwitch(now time.Time) string {
	return l.local.rotate(now)
}

// observe records a peer's advertised receive queue. changed is false for the
// first advertisement and true only when an established peer lane rotates.
func (l *peerLane) observe(queue string) (changed bool, err error) {
	collectAt, present, err := queues.GCActivation(queue)
	if err != nil {
		return false, fmt.Errorf("parse reply queue %q: %w", queue, err)
	}
	if !present || collectAt.IsZero() {
		return false, fmt.Errorf("reply queue %q has no finite gc deadline", queue)
	}
	changed = l.queue != "" && queue != l.queue
	l.queue = queue
	l.collectAt = collectAt
	return changed, nil
}

func (l *peerLane) forceAt(timing laneTiming) time.Time {
	return l.collectAt.Add(-timing.forceBefore)
}

func sessionQueue(prefix, session string, collectAt time.Time, direction string) string {
	return path.Join(prefix,
		fmt.Sprintf("sess=%s;gc=%d", queues.EscapeComponent(session), collectAt.Unix()),
		direction,
	)
}

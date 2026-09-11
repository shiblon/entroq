package async

import (
	"testing"
	"time"
)

func TestReceiveLaneRotationThresholds(t *testing.T) {
	timing := defaultLaneTiming
	started := time.Unix(2_000_000_000, 0).UTC()
	lane := newReceiveLane("/service", "abc", "response", started, timing)

	if got, want := lane.collectAt, started.Add(30*time.Minute); !got.Equal(want) {
		t.Fatalf("collectAt: got %v, want %v", got, want)
	}
	if lane.shouldPiggyback(lane.collectAt.Add(-10*time.Minute - time.Nanosecond)) {
		t.Error("piggybacked before the ten-minute threshold")
	}
	if !lane.shouldPiggyback(lane.collectAt.Add(-10 * time.Minute)) {
		t.Error("did not piggyback at the ten-minute threshold")
	}
	if got, want := lane.forceAt(), lane.collectAt.Add(-5*time.Minute); !got.Equal(want) {
		t.Errorf("forceAt: got %v, want %v", got, want)
	}

	oldQueue := lane.queue
	lane.rotate(lane.collectAt.Add(-9 * time.Minute))
	if lane.queue == oldQueue {
		t.Fatal("rotation did not change the queue")
	}
	if lane.shouldPiggyback(lane.collectAt.Add(-10*time.Minute - time.Nanosecond)) {
		t.Error("new generation retained the old piggyback threshold")
	}
}

func TestPeerLaneRequiresFiniteGCDeadline(t *testing.T) {
	var lane peerLane
	if _, err := lane.observe("/service/reply"); err == nil {
		t.Error("queue without gc deadline was accepted")
	}
	if _, err := lane.observe("/service/gc=/reply"); err == nil {
		t.Error("queue with always-active gc was accepted")
	}

	changed, err := lane.observe("/service/sess=abc;gc=2000000000/response")
	if err != nil {
		t.Fatalf("observe initial queue: %v", err)
	}
	if changed {
		t.Error("initial queue was reported as a switch")
	}
	changed, err = lane.observe("/service/sess=abc;gc=2000000100/response")
	if err != nil {
		t.Fatalf("observe switched queue: %v", err)
	}
	if !changed {
		t.Error("replacement queue was not reported as a switch")
	}
}

func TestSessionQueueEscapesSessionPolicyCharacters(t *testing.T) {
	got := sessionQueue("/service", `a/b;c\\d`, time.Unix(123, 0), "request")
	want := `/service/sess=a\/b\;c\\\\d;gc=123/request`
	if got != want {
		t.Errorf("session queue: got %q, want %q", got, want)
	}
}

func TestSessionLanesCompleteOneSwitchExchange(t *testing.T) {
	timing := defaultLaneTiming
	now := time.Unix(2_000_000_000, 0).UTC()
	a := sessionLanes{local: newReceiveLane("/a", "abc", "response", now, timing)}
	b := sessionLanes{local: newReceiveLane("/b", "abc", "request", now, timing)}

	if reciprocate, err := a.observePeer(b.local.queue); err != nil || reciprocate {
		t.Fatalf("A initial peer: reciprocate=%v err=%v", reciprocate, err)
	}
	if reciprocate, err := b.observePeer(a.local.queue); err != nil || reciprocate {
		t.Fatalf("B initial peer: reciprocate=%v err=%v", reciprocate, err)
	}

	oldB := b.local.queue
	b.initiateSwitch(now.Add(20 * time.Minute))
	if b.local.queue == oldB || !b.awaitingSwitch {
		t.Fatal("B did not initiate its switch")
	}
	if reciprocate, err := a.observePeer(b.local.queue); err != nil || !reciprocate {
		t.Fatalf("A did not request a reciprocal switch: reciprocate=%v err=%v", reciprocate, err)
	}

	oldA := a.local.queue
	a.reciprocateSwitch(now.Add(20 * time.Minute))
	if a.local.queue == oldA || a.awaitingSwitch {
		t.Fatal("A reciprocal switch incorrectly awaited another reply")
	}
	if reciprocate, err := b.observePeer(a.local.queue); err != nil || reciprocate {
		t.Fatalf("B treated the reciprocal as a new switch: reciprocate=%v err=%v", reciprocate, err)
	}
	if b.awaitingSwitch {
		t.Fatal("B remained awaiting after the reciprocal switch")
	}
}

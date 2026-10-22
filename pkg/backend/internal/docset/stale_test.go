package docset

import (
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// TestStaleRenewalStillHolds pins the reason a doc arrival is a duration.
//
// A renewal is evaluated some time after the caller issued it. While the
// caller sent an instant, one delayed past its own lease was no longer in the
// future when the backend looked, and an arrival that is not in the future
// means release -- so a renewal merely late handed the set away and reported
// success doing it. A duration cannot invert that way: the backend resolves it
// against its own now, so lateness postpones the arrival and nothing more.
func TestStaleRenewalStillHolds(t *testing.T) {
	s := newStore(Lock{Claimant: "me", At: now.Add(time.Minute)})
	const lease = 300 * time.Millisecond
	// Evaluated a full second after the renewal was asked for: longer than the
	// lease, which is what used to invert it.
	late := now.Add(time.Second)

	mod := entroq.NewModification("me",
		entroq.Arriving(entroq.ReadyIn(lease).Docs(
			&entroq.DocSet{Namespace: "ns", Key: "k", Version: 5})))

	// The heart of it: the instruction does not decay in transit. An instant
	// would have aged into the past by now; the duration is still the lease.
	if got := mod.DocArrives[0].By; got != lease {
		t.Errorf("arrival in flight is %v, want %v unchanged", got, lease)
	}

	p := Evaluate(mod, late, s.member, s.occupant, s.lock)
	if p.Err != nil {
		t.Fatalf("Evaluate: %v", p.Err)
	}

	got := p.Locks[set]
	if got.Claimant != "me" {
		t.Errorf("a renewal evaluated %v late released the set: claimant %q, want %q", time.Second, got.Claimant, "me")
	}
	if want := late.Add(lease); !got.At.Equal(want) {
		t.Errorf("renewed arrival is %v, want %v: an arrival resolves against the backend's now, not the caller's", got.At, want)
	}
}

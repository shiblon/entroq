package handoffworker

import (
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/queues"
)

// TestDefaultGraveyardGCEligible confirms the default tombstone queue opts into
// garbage collection via its name, so the destination backend's built-in GC
// reaps crash orphans without a separate reaper. Actual collection is exercised
// by each backend's own GC tests; here we only pin the naming contract.
func TestDefaultGraveyardGCEligible(t *testing.T) {
	q := defaultGraveyard("/svc/inbox")
	at, present, err := queues.GCActivation(q)
	if err != nil {
		t.Fatalf("GCActivation(%q): %v", q, err)
	}
	if !present {
		t.Fatalf("graveyard %q is not GC-eligible; the backend would never reap it", q)
	}
	if at.After(time.Now()) {
		t.Errorf("graveyard %q should be collectable now (gc=0), got activate-at %v", q, at)
	}
}

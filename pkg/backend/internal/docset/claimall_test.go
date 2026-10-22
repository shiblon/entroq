package docset

import (
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// TestClaimAllHoldThatDoesNotReachPastNow covers what a hold in the past is
// blamed on, which is the difference between a caller losing one task and a
// caller losing its process.
//
// A claim matching a task takes the task's own stored arrival as its hold
// (entroq.MatchingLeaseOf), so a caller that ran past its lease arrives with a
// hold already behind it without having asked for anything wrong. That is a
// failed depend on the task. A caller that names a non-positive duration
// itself did ask for something wrong.
func TestClaimAllHoldThatDoesNotReachPastNow(t *testing.T) {
	g := Set{Namespace: "ns", Key: "k"}
	lock := func(Set) Lock { return Absent }
	members := func(Set) ([]*entroq.Doc, error) { return nil, nil }
	task := &entroq.TaskID{ID: "task-1", Version: 3}

	for _, tc := range []struct {
		name  string
		cq    *entroq.DocClaim
		until time.Time
		want  func(error) bool
		wants string
	}{
		{
			name:  "a matched task whose lease ran out",
			cq:    &entroq.DocClaim{Sets: []*entroq.DocSetClaim{{Namespace: g.Namespace, Key: g.Key}}, TaskToMatch: task},
			until: now.Add(-time.Second),
			want:  entroq.IsDependency,
			wants: "a dependency error",
		},
		{
			name:  "a matched task arriving exactly now",
			cq:    &entroq.DocClaim{Sets: []*entroq.DocSetClaim{{Namespace: g.Namespace, Key: g.Key}}, TaskToMatch: task},
			until: now,
			want:  entroq.IsDependency,
			wants: "a dependency error",
		},
		{
			name:  "a caller's own duration of zero",
			cq:    &entroq.DocClaim{Sets: []*entroq.DocSetClaim{{Namespace: g.Namespace, Key: g.Key}}},
			until: now,
			want:  entroq.IsInvalidArgument,
			wants: "an invalid argument",
		},
		{
			name:  "a caller's own negative duration",
			cq:    &entroq.DocClaim{Sets: []*entroq.DocSetClaim{{Namespace: g.Namespace, Key: g.Key}}, Duration: -time.Minute},
			until: now.Add(-time.Minute),
			want:  entroq.IsInvalidArgument,
			wants: "an invalid argument",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ClaimAll(tc.cq, now, tc.until, lock, members)
			if err == nil {
				t.Fatalf("Claim held until %v at %v: want %s, got no error", tc.until, now, tc.wants)
			}
			if !tc.want(err) {
				t.Fatalf("Claim held until %v at %v: want %s, got %v", tc.until, now, tc.wants, err)
			}
		})
	}

	t.Run("the lapsed task is named as the failed depend", func(t *testing.T) {
		cq := &entroq.DocClaim{Sets: []*entroq.DocSetClaim{{Namespace: g.Namespace, Key: g.Key}}, TaskToMatch: task}
		_, err := ClaimAll(cq, now, now.Add(-time.Second), lock, members)
		depErr, ok := entroq.AsDependency(err)
		if !ok {
			t.Fatalf("Claim matching a lapsed lease: want a dependency error, got %v", err)
		}
		// The task, not a set: a caller can tell its own lapsed lease from
		// someone else holding what it asked for, which end differently.
		if !depErr.HasMissing() {
			t.Errorf("Claim matching a lapsed lease: want the task among the missing depends, got %v", depErr)
		}
		if depErr.HasClaimedDocs() || depErr.HasMissingDocs() {
			t.Errorf("Claim matching a lapsed lease: want no doc blamed, got %v", depErr)
		}
		if len(depErr.Depends) != 1 || depErr.Depends[0].ID != task.ID {
			t.Errorf("Claim matching a lapsed lease: want task %q among the depends, got %v", task.ID, depErr.Depends)
		}
	})
}

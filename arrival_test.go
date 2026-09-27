package entroq

import (
	"testing"
	"time"
)

func TestArrivingBuildsArrivals(t *testing.T) {
	task := &Task{ID: "t", Version: 3, Queue: "q"}
	set := &DocSet{Namespace: "ns", Key: "k", Version: 5, NumDocs: 2, Docs: []*Doc{{ID: "a"}}}
	mod := NewModification("me", Arriving(ReadyIn(time.Minute).Tasks(task), ReadyNow().Docs(set)))
	if len(mod.Arrives) != 1 || len(mod.DocArrives) != 1 {
		t.Fatalf("Modification: got %v", mod)
	}
	if got := mod.Arrives[0]; got.TaskID != (TaskID{ID: "t", Version: 3, Queue: "q"}) || time.Until(got.At) < 59*time.Second {
		t.Errorf("Task arrival: want task t ready in a minute, got %+v", got)
	}
	if got := mod.DocArrives[0]; got.DocSetID != (DocSetID{Namespace: "ns", Key: "k", Version: 5}) || !got.At.IsZero() {
		t.Errorf("Set arrival: want set ns/k ready now, got %+v", got)
	}
}

func TestArrivalsValidate(t *testing.T) {
	task := &Task{ID: "t", Queue: "q"}
	set := &DocSet{Namespace: "ns", Key: "k"}
	for _, tc := range []struct {
		name  string
		args  []ModifyArg
		valid bool
	}{
		{"task", []ModifyArg{Arriving(ReadyNow().Tasks(task))}, true},
		{"set", []ModifyArg{Arriving(ReadyNow().Docs(set))}, true},
		{"with other work", []ModifyArg{Arriving(ReadyNow().Docs(set)), InsertingInto("q")}, true},
		{"task with no queue", []ModifyArg{Arriving(ReadyNow().Tasks(&Task{ID: "t"}))}, false},
		{"task twice", []ModifyArg{Arriving(ReadyNow().Tasks(task), ReadyIn(time.Second).Tasks(task))}, false},
		{"task arriving and deleted", []ModifyArg{Arriving(ReadyNow().Tasks(task)), task.Delete()}, false},
		{"set twice", []ModifyArg{Arriving(ReadyNow().Docs(set, set))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mod := NewModification("me", tc.args...)
			err := mod.EnsureModifyKeys()
			if err == nil {
				_, _, err = mod.AllDependencies()
			}
			if tc.valid && err != nil {
				t.Errorf("Validate: %v", err)
			}
			if !tc.valid && !IsInvalidArgument(err) {
				t.Errorf("Validate: want an invalid argument, got %v", err)
			}
		})
	}
}

func TestDependencyErrorGroups(t *testing.T) {
	held := &DocSet{Namespace: "ns", Key: "k", Version: 2, Claimant: "them"}
	a := &DependencyError{SetClaims: []*DocSet{held}}
	b := &DependencyError{SetClaims: []*DocSet{held}, DocArrives: []*DocSet{{Namespace: "ns", Key: "other"}}}
	m := a.Merge(b)
	if len(m.SetClaims) != 1 || len(m.DocArrives) != 1 {
		t.Errorf("Merge: want one set of each, got %v", m)
	}
	if !m.HasClaimedDocs() || !m.HasMissingDocs() || !m.HasAny() {
		t.Errorf("Set failures: want them to count as claimed and missing docs, got %v", m)
	}
	if c := m.Copy(); len(c.SetClaims) != 1 || len(c.DocArrives) != 1 {
		t.Errorf("Copy: got %v", c)
	}
}

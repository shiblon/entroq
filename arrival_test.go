package entroq

import (
	"testing"
	"time"
)

func TestArrivingBuildsArrivals(t *testing.T) {
	task := &Task{ID: "t", Version: 3, Queue: "q"}
	group := &DocGroup{Namespace: "ns", Key: "k", Version: 5, NumDocs: 2, Docs: []*Doc{{ID: "a"}}}
	mod := NewModification("me", Arriving(ReadyIn(time.Minute).Tasks(task), ReadyNow().Docs(group)))
	if len(mod.Arrives) != 1 || len(mod.DocArrives) != 1 {
		t.Fatalf("Modification: got %v", mod)
	}
	if got := mod.Arrives[0]; got.TaskID != (TaskID{ID: "t", Version: 3, Queue: "q"}) || time.Until(got.At) < 59*time.Second {
		t.Errorf("Task arrival: want task t ready in a minute, got %+v", got)
	}
	if got := mod.DocArrives[0]; got.DocSetID != (DocSetID{Namespace: "ns", Key: "k", Version: 5}) || !got.At.IsZero() {
		t.Errorf("Group arrival: want group ns/k ready now, got %+v", got)
	}
}

func TestArrivalsValidate(t *testing.T) {
	task := &Task{ID: "t", Queue: "q"}
	group := &DocGroup{Namespace: "ns", Key: "k"}
	for _, tc := range []struct {
		name  string
		args  []ModifyArg
		valid bool
	}{
		{"task", []ModifyArg{Arriving(ReadyNow().Tasks(task))}, true},
		{"group", []ModifyArg{Arriving(ReadyNow().Docs(group))}, true},
		{"with other work", []ModifyArg{Arriving(ReadyNow().Docs(group)), InsertingInto("q")}, true},
		{"task with no queue", []ModifyArg{Arriving(ReadyNow().Tasks(&Task{ID: "t"}))}, false},
		{"task twice", []ModifyArg{Arriving(ReadyNow().Tasks(task), ReadyIn(time.Second).Tasks(task))}, false},
		{"task arriving and deleted", []ModifyArg{Arriving(ReadyNow().Tasks(task)), task.Delete()}, false},
		{"group twice", []ModifyArg{Arriving(ReadyNow().Docs(group, group))}, false},
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
	held := &DocGroup{Namespace: "ns", Key: "k", Version: 2, Claimant: "them"}
	a := &DependencyError{GroupClaims: []*DocGroup{held}}
	b := &DependencyError{GroupClaims: []*DocGroup{held}, DocArrives: []*DocGroup{{Namespace: "ns", Key: "other"}}}
	m := a.Merge(b)
	if len(m.GroupClaims) != 1 || len(m.DocArrives) != 1 {
		t.Errorf("Merge: want one group of each, got %v", m)
	}
	if !m.HasClaimedDocs() || !m.HasMissingDocs() || !m.HasAny() {
		t.Errorf("Group failures: want them to count as claimed and missing docs, got %v", m)
	}
	if c := m.Copy(); len(c.GroupClaims) != 1 || len(c.DocArrives) != 1 {
		t.Errorf("Copy: got %v", c)
	}
}

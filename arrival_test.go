package entroq

import (
	"reflect"
	"strings"
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
	if got := mod.DocArrives[0]; got.DocID != *NewDocSetRef("ns", "k", 5) || !got.At.IsZero() {
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
		{"set with no key", []ModifyArg{Arriving(ReadyNow().Docs(&DocSet{Namespace: "ns"}))}, false},
		{"doc, not a set", []ModifyArg{func(m *Modification) {
			m.DocArrives = append(m.DocArrives, &DocArrival{DocID: *NewDocID("ns", "d", 0)})
		}}, false},
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

func TestDependencyErrorSets(t *testing.T) {
	held := NewDocSetRef("ns", "k", 2)
	// A doc whose ID is the set's key is a different thing from the set.
	doc := NewDocID("ns", "k", 2)
	a := &DependencyError{DocClaims: []*DocID{held}}
	b := &DependencyError{DocClaims: []*DocID{held, doc}, DocArrives: []*DocID{NewDocSetRef("ns", "other", 0)}}
	m := a.Merge(b)
	if len(m.DocClaims) != 2 || len(m.DocArrives) != 1 {
		t.Errorf("Merge: want the set and the doc held, and one arrival, got %v", m)
	}
	if !m.HasClaimedDocs() || !m.HasMissingDocs() || !m.HasAny() {
		t.Errorf("Set failures: want them to count as claimed and missing docs, got %v", m)
	}
	if c := m.Copy(); len(c.DocClaims) != 2 || len(c.DocArrives) != 1 {
		t.Errorf("Copy: got %v", c)
	}
}

func TestDocIDIsSetRef(t *testing.T) {
	if !NewDocSetRef("ns", "k", 1).IsSetRef() {
		t.Error("Set reference: want IsSetRef")
	}
	if NewDocID("ns", "id", 1).IsSetRef() {
		t.Error("Doc reference: want not IsSetRef")
	}
	if got, want := NewDocSetRef("ns", "k", 1).String(), "ns/[k]:v1"; got != want {
		t.Errorf("Set reference string: got %q, want %q", got, want)
	}
}

// TestWithModificationCopiesEveryList checks that WithModification carries
// every operation list of a Modification, so one added later cannot be
// dropped without notice: a modification of only arrivals once became an
// empty one.
func TestWithModificationCopiesEveryList(t *testing.T) {
	src := new(Modification)
	v := reflect.ValueOf(src).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !v.Type().Field(i).IsExported() || f.Kind() != reflect.Slice {
			continue
		}
		f.Set(reflect.MakeSlice(f.Type(), 1, 1))
	}
	dest := NewModification("", WithModification(src))
	d := reflect.ValueOf(dest).Elem()
	for i := 0; i < v.NumField(); i++ {
		if !v.Type().Field(i).IsExported() || v.Field(i).Kind() != reflect.Slice {
			continue
		}
		if d.Field(i).Len() != 1 {
			t.Errorf("WithModification dropped %s", v.Type().Field(i).Name)
		}
	}
}

// TestIsEmptyCountsEveryList checks that a modification with any one
// operation list filled is not empty, so a list added later cannot be
// refused as nothing to do.
func TestIsEmptyCountsEveryList(t *testing.T) {
	if !new(Modification).IsEmpty() {
		t.Error("A modification with no operations: want empty")
	}
	typ := reflect.TypeOf(Modification{})
	for i := 0; i < typ.NumField(); i++ {
		if !typ.Field(i).IsExported() || typ.Field(i).Type.Kind() != reflect.Slice {
			continue
		}
		m := new(Modification)
		f := reflect.ValueOf(m).Elem().Field(i)
		f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		if m.IsEmpty() {
			t.Errorf("A modification with one %s: want it not empty", typ.Field(i).Name)
		}
	}
}

// TestResettingClaims checks that a claims reset is recorded for its change, and
// carried by WithModification.
func TestResettingClaims(t *testing.T) {
	a, b := &Task{ID: "a", Queue: "q"}, &Task{ID: "b", Queue: "q"}
	src := NewModification("", a.Change(ResettingClaims()), b.Change())
	if !src.ResetsClaims("a") || src.ResetsClaims("b") {
		t.Errorf("Resets: want a's change to reset claims and b's not")
	}
	if dest := NewModification("", WithModification(src)); !dest.ResetsClaims("a") || dest.ResetsClaims("b") {
		t.Error("WithModification: want the reset carried over")
	}
}

// TestModificationStringShowsResets checks that a modification's claim resets
// show when it is logged, since they are not among its operation lists.
func TestModificationStringShowsResets(t *testing.T) {
	m := NewModification("", (&Task{ID: "a", Queue: "q"}).Change(ResettingClaims()))
	if s := m.String(); !strings.Contains(s, "reset-claims: [a]") {
		t.Errorf("String: want the reset of a shown, got %q", s)
	}
}

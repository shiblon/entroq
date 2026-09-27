package arrival

import (
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

func TestChanges(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	stored := &entroq.Task{ID: "t", Queue: "q", Version: 3, Value: []byte(`"v"`), Attempt: 2, Err: "e", Claims: 4}
	mod := entroq.NewModification("me",
		entroq.Arriving(entroq.ReadyIn(time.Minute).Tasks(stored), entroq.ReadyNow().Tasks(&entroq.Task{ID: "gone", Queue: "q"})),
		entroq.InsertingInto("q"),
	)
	mod.Arrives[0].At = now.Add(time.Minute) // as if made at now
	got := Changes(mod, now, func(id string) *entroq.Task {
		if id == "t" {
			return stored
		}
		return nil
	})
	if len(got.Arrives) != 0 || len(got.Changes) != 2 || len(got.Inserts) != 1 {
		t.Fatalf("Changes: want two changes, no arrivals, the insert kept, got %v", got)
	}
	c := got.Changes[0]
	if c.Version != 3 || c.Queue != "q" || c.FromQueue != "q" || !c.At.Equal(now.Add(time.Minute)) ||
		string(c.Value) != `"v"` || c.Attempt != 2 || c.Err != "e" {
		t.Errorf("Arrival change: want the stored task ready in a minute, got %+v", c)
	}
	if c := got.Changes[1]; c.ID != "gone" || !c.At.Equal(now) || c.Value != nil {
		t.Errorf("Missing task: want a bare change ready now, got %+v", c)
	}
	if len(mod.Arrives) != 2 || len(mod.Changes) != 0 {
		t.Errorf("Changes must not alter its argument, got %v", mod)
	}
}

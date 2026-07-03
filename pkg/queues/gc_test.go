package queues

import (
	"fmt"
	"testing"
	"time"
)

func TestParseGCActivation(t *testing.T) {
	rfc := time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC)

	cases := []struct {
		name    string
		value   string
		want    time.Time // ignored when wantErr is true
		wantErr bool
	}{
		{
			name:  "empty is always active",
			value: "",
			want:  time.Time{},
		},
		{
			name:  "zero is epoch",
			value: "0",
			want:  time.Unix(0, 0).UTC(),
		},
		{
			name:  "unix seconds",
			value: "1719000000",
			want:  time.Unix(1719000000, 0).UTC(),
		},
		{
			name:  "rfc3339",
			value: "2026-07-02T15:04:05Z",
			want:  rfc,
		},
		{
			name:  "rfc3339 fractional seconds (JS toISOString)",
			value: "2026-07-02T15:04:05.000Z",
			want:  rfc,
		},
		{
			name:    "not a timestamp",
			value:   "someday",
			wantErr: true,
		},
		{
			name:    "malformed rfc3339",
			value:   "2026-13-40T99:99:99Z",
			wantErr: true,
		},
		{
			name:    "decimal is not unix and not rfc3339",
			value:   "12.5",
			wantErr: true,
		},
	}

	for _, test := range cases {
		got, err := ParseGCActivation(test.value)
		if test.wantErr {
			if err == nil {
				t.Errorf("TestParseGCActivation %q: wanted error, got %v", test.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("TestParseGCActivation %q: unexpected error: %v", test.name, err)
			continue
		}
		if !got.Equal(test.want) {
			t.Errorf("TestParseGCActivation %q: wanted %v, got %v", test.name, test.want, got)
		}
	}
}

func TestGCActivation(t *testing.T) {
	cases := []struct {
		name    string
		queue   string
		present bool
		want    time.Time // ignored unless present && !wantErr
		wantErr bool
	}{
		{
			name:  "no gc key",
			queue: "/some/path/somewhere",
		},
		{
			name:    "canonical gc always on",
			queue:   "/tasks/gc=0",
			present: true,
			want:    time.Unix(0, 0).UTC(),
		},
		{
			name:    "empty value is always on",
			queue:   "/tasks/gc=/leaf",
			present: true,
			want:    time.Time{},
		},
		{
			name:  "exp is ignored",
			queue: "/tasks/response/exp=1719000000",
		},
		{
			name:    "last gc wins",
			queue:   "/gc=100/sub/gc=200",
			present: true,
			want:    time.Unix(200, 0).UTC(),
		},
		{
			name:    "rfc3339 value",
			queue:   "/tasks/gc=2026-07-02T15:04:05Z",
			present: true,
			want:    time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
		},
		{
			name:    "malformed value present but errors",
			queue:   "/tasks/gc=whenever",
			present: true,
			wantErr: true,
		},
	}

	for _, test := range cases {
		got, present, err := GCActivation(test.queue)
		if present != test.present {
			t.Errorf("TestGCActivation %q: wanted present=%v, got %v", test.name, test.present, present)
		}
		if test.wantErr {
			if err == nil {
				t.Errorf("TestGCActivation %q: wanted error, got %v", test.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("TestGCActivation %q: unexpected error: %v", test.name, err)
			continue
		}
		if test.present && !got.Equal(test.want) {
			t.Errorf("TestGCActivation %q: wanted %v, got %v", test.name, test.want, got)
		}
	}
}

func TestPathLabels(t *testing.T) {
	cases := []struct {
		name       string
		queue      string
		l1, l2, l3 string
	}{
		{
			name:  "empty",
			queue: "",
		},
		{
			name:  "one level",
			queue: "/a",
			l1:    "/a",
		},
		{
			name:  "two levels",
			queue: "/a/b",
			l1:    "/a",
			l2:    "/a/b",
		},
		{
			name:  "three levels",
			queue: "/a/b/c",
			l1:    "/a",
			l2:    "/a/b",
			l3:    "/a/b/c",
		},
		{
			name:  "fourth level ignored",
			queue: "/a/b/c/d",
			l1:    "/a",
			l2:    "/a/b",
			l3:    "/a/b/c",
		},
		{
			name:  "escaped slash stays within a level",
			queue: "/a\\/x/b",
			l1:    "/a/x",
			l2:    "/a/x/b",
		},
	}

	for _, test := range cases {
		l1, l2, l3 := PathLabels(test.queue)
		if l1 != test.l1 || l2 != test.l2 || l3 != test.l3 {
			t.Errorf("TestPathLabels %q: wanted (%q, %q, %q), got (%q, %q, %q)",
				test.name, test.l1, test.l2, test.l3, l1, l2, l3)
		}
	}
}

func ExampleGCActivation() {
	// The most specific (last) gc component wins.
	at, present, _ := GCActivation("/tasks/gc=100/response/gc=200")
	fmt.Printf("present=%v activateAt=%d\n", present, at.Unix())
	// Output: present=true activateAt=200
}

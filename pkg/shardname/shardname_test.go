package shardname

import (
	"reflect"
	"testing"
)

func TestName(t *testing.T) {
	for _, tc := range []struct {
		base string
		i, n int
		want string
	}{
		{"out", 3, 8, "out-00003-of-00008"},
		{"dir/out", 0, 1, "dir/out-00000-of-00001"},
		{"out", 7, 123456, "out-000007-of-123456"},
	} {
		if got := Name(tc.base, tc.i, tc.n); got != tc.want {
			t.Errorf("Name(%q, %d, %d) = %q, want %q", tc.base, tc.i, tc.n, got, tc.want)
		}
	}
}

func TestExpand(t *testing.T) {
	got, err := Expand("logs/a@b@3")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := []string{"logs/a@b-00000-of-00003", "logs/a@b-00001-of-00003", "logs/a@b-00002-of-00003"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand = %q, want %q", got, want)
	}
	for _, bad := range []string{"out", "@3", "out@0", "out@-1", "out@x", "out@03", "out@"} {
		if _, err := Expand(bad); err == nil {
			t.Errorf("Expand(%q) succeeded, want an error", bad)
		}
	}
}

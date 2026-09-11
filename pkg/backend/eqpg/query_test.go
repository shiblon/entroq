package eqpg

import "testing"

func TestLikePrefix(t *testing.T) {
	tests := map[string]string{
		"queue": `queue%`,
		`q%`:    `q\%%`,
		`q_`:    `q\_%`,
		`q\x`:   `q\\x%`,
	}
	for input, want := range tests {
		if got := likePrefix(input); got != want {
			t.Errorf("likePrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

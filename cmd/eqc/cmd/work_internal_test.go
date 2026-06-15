package cmd

import (
	"io"
	"strings"
	"testing"
)

func TestCappedBufferReportsOverflow(t *testing.T) {
	buf := &cappedBuffer{max: 3}
	n, err := io.Copy(buf, strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != int64(len(`{"a":1}`)) {
		t.Fatalf("copy length: got %d, want %d", n, len(`{"a":1}`))
	}
	if !buf.exceeded {
		t.Fatal("expected buffer to report overflow")
	}
	if got, want := buf.String(), `{"a`; got != want {
		t.Fatalf("buffer contents: got %q, want %q", got, want)
	}
}

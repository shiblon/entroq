package stuffedio

import (
	"bytes"
	"fmt"
	"log"
	"testing"
)

func Example() {
	buf := new(bytes.Buffer)

	w := NewWriter(buf)
	if err := w.Append([]byte("A very short message")); err != nil {
		log.Fatalf("Error appending: %v", err)
	}

	fmt.Printf("%q\n", string(buf.Bytes()))

	r := NewReader(buf)
	for !r.Done() {
		b, err := r.Next()
		if err != nil {
			log.Fatalf("Error reading: %v", err)
		}
		fmt.Println(string(b))
	}

	// Output:
	// "\xfe\xfd\x14A very short message"
	// A very short message
}

func TestWriter_Append_short(t *testing.T) {
	cases := []struct {
		name  string
		write string
		raw   string
		want  string
	}{
		{
			name:  "simple",
			write: "Short message",
			raw:   "\xfe\xfd\x0dShort message",
			want:  "Short message",
		},
		{
			name:  "with-delimiters",
			write: "short\xfe\xfd",
			raw:   "\xfe\xfd\x05short",
			want:  "short\xfe\xfd",
		},
	}

	for _, test := range cases {
		buf := new(bytes.Buffer)
		w := NewWriter(buf)
		if err := w.Append([]byte(test.write)); err != nil {
			t.Fatalf("Append_short %q: writing: %v", test.name, err)
		}
		if want, got := []byte(test.raw), buf.Bytes(); !bytes.Equal(want, got) {
			t.Fatalf("Append_short %q: want raw %q, got %q", test.name, string(want), string(got))
		}
		r := NewReader(buf)
		b, err := r.Next()
		if err != nil {
			t.Fatalf("Append_short %q: reading: %v", test.name, err)
		}
		if want, got := test.want, string(b); want != got {
			t.Errorf("Append_short %q: wanted read %q, got %q", test.name, want, got)
		}
	}
}

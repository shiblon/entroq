package stuffedio

import (
	"bytes"
	"fmt"
	"log"
	"testing"
)

func ExampleWriter_Append() {
	buf := new(bytes.Buffer)

	w := NewWriter(buf)
	if err := w.Append([]byte("A very short message")); err != nil {
		log.Fatalf("Error appending: %v", err)
	}

	fmt.Printf("%q\n", string(buf.Bytes()))

	// Output:
	// "\xfe\xfd\x14A very short message"
}

func ExampleReader() {
	r := NewReader(bytes.NewBuffer([]byte("\xfe\xfd\x14A very short message\x00\x00\xfe\xfd\x05hello")))
	for !r.Done() {
		b, err := r.Next()
		if err != nil {
			log.Fatalf("Error reading: %v", err)
		}
		fmt.Printf("%q\n", string(b))
	}

	// Output:
	// "A very short message\xfe\xfd"
	// "hello"
}

func TestWriter_Append_one(t *testing.T) {
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
		},
		{
			name:  "tail-delimiters",
			write: "short\xfe\xfd",
			raw:   "\xfe\xfd\x05short\x00\x00",
		},
		{
			name:  "leading-delimiters",
			write: "\xfe\xfdshort",
			raw:   "\xfe\xfd\x00\x05\x00short",
		},
		{
			name:  "middle-delimiters",
			write: "short\xfe\xfdmessage",
			raw:   "\xfe\xfd\x05short\x07\x00message",
		},
		{
			name:  "longer-message",
			write: "This is a much longer message, containing many more characters than can fit into a single short message of 252 characters. It kind of rambles on, as a result. Good luck figuring out where the break needs to be! It turns out that 252 bytes is really quite a lot of text for a short test like this.",
			raw:   "\xfe\xfd\xfcThis is a much longer message, containing many more characters than can fit into a single short message of 252 characters. It kind of rambles on, as a result. Good luck figuring out where the break needs to be! It turns out that 252 bytes is really qui\x2c\x00te a lot of text for a short test like this.",
		},
		{
			name:  "longer-message-with-delimiter",
			write: "This is a much longer message, containing many more characters than can fit into a single short message of 252 characters. It kind of rambles on, as a result. Good luck figuring out where the break needs to be! It turns out that 252 bytes is really quite a lot of text for a short test like this.\xfe\xfd",
			raw:   "\xfe\xfd\xfcThis is a much longer message, containing many more characters than can fit into a single short message of 252 characters. It kind of rambles on, as a result. Good luck figuring out where the break needs to be! It turns out that 252 bytes is really qui\x2c\x00te a lot of text for a short test like this.\x00\x00",
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
		if want, got := test.write, string(b); want != got {
			t.Errorf("Append_short %q: wanted read %q, got %q", test.name, want, got)
		}
	}
}

func TestReader_Next(t *testing.T) {
}

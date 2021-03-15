package stuffedio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"

	"github.com/google/go-cmp/cmp"
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
			name:  "partial-delimiters",
			write: "short\xfemessage",
			raw:   "\xfe\xfd\x0dshort\xfemessage",
		},
		{
			name:  "second-half-delimiters",
			write: "short\xfdmessage",
			raw:   "\xfe\xfd\x0dshort\xfdmessage",
		},
		{
			name:  "empty",
			write: "",
			raw:   "",
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
		{
			name:  "longer-message-with-split-delimiter",
			write: "This is a much longer message, containing many more characters than can fit into a single short message of 252 characters. It kind of rambles on, as a result. Good luck figuring out where the break needs to be! It turns out that 252 bytes is really qu\xfe\xfdite a lot of text for a short test like this.\xfe\xfd",
			raw:   "\xfe\xfd\xfcThis is a much longer message, containing many more characters than can fit into a single short message of 252 characters. It kind of rambles on, as a result. Good luck figuring out where the break needs to be! It turns out that 252 bytes is really qu\xfe\x2e\x00\xfdite a lot of text for a short test like this.\x00\x00",
		},
	}

	for _, test := range cases {
		buf := new(bytes.Buffer)
		w := NewWriter(buf)
		if err := w.Append([]byte(test.write)); err != nil {
			t.Fatalf("Append_one %q: writing: %v", test.name, err)
		}
		if want, got := []byte(test.raw), buf.Bytes(); !bytes.Equal(want, got) {
			t.Fatalf("Append_one %q: want raw %q, got %q", test.name, string(want), string(got))
		}
		r := NewReader(buf)
		b, err := r.Next()
		switch {
		case test.raw == "" && !errors.Is(err, io.EOF):
			t.Fatalf("Append_one %q: expected EOF, got %v with value %q", test.name, err, string(b))
		case test.raw == "" && errors.Is(err, io.EOF):
			// Do nothing
		case err != nil:
			t.Fatalf("Append_one %q: reading: %v", test.name, err)
		}
		if want, got := test.write, string(b); want != got {
			t.Errorf("Append_one %q: wanted read %q, got %q", test.name, want, got)
		}
	}
}

func TestWriter_Append_multiple(t *testing.T) {
	cases := []struct {
		name  string
		write []string
	}{
		{
			name:  "simple",
			write: []string{"hello", "there", "person"},
		},
		{
			name:  "with delimiters",
			write: []string{"hello\xfe\xfd", "\xfe\xfdthere", "per\xfe\xfdson", "hey", "mul\xfe\xfdtiple\xfe\xfddelimiters"},
		},
	}

	for _, test := range cases {
		buf := new(bytes.Buffer)
		w := NewWriter(buf)
		for _, val := range test.write {
			if err := w.Append([]byte(val)); err != nil {
				t.Fatalf("Append_multiple %q: %v", test.name, err)
			}
		}

		var got []string
		r := NewReader(buf)
		for !r.Done() {
			b, err := r.Next()
			if err != nil {
				t.Fatalf("Append_multiple %q: %v", test.name, err)
			}
			got = append(got, string(b))
		}

		if diff := cmp.Diff(test.write, got); diff != "" {
			t.Errorf("Append_multiple: %q unexpected diff (+got -want):\n%v", test.name, diff)
		}
	}
}

func TestReader_Next_corrupt(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string // empty means "expect a corruption error" for this test sequence
	}{
		{
			name: "missing-delimiter",
			raw:  "hello",
			want: []string{""},
		},
		{
			name: "garbage-in-front",
			raw:  "hello\xfe\xfd\x01A",
			want: []string{"", "A"},
		},
		{
			name: "garbage-in-back",
			raw:  "\xfe\xfd\x02AB\xfe\xfdfooey",
			want: []string{"AB", ""},
		},
		{
			name: "garbage-in-middle",
			raw:  "\xfe\xfd\x02AB\xfe\xfdrandom crap\xfe\xfd\x02BC\xfe\xfd\x02CD",
			want: []string{"AB", "", "BC", "CD"},
		},
	}

	for _, test := range cases {
		buf := bytes.NewBuffer([]byte(test.raw))
		r := NewReader(buf)
		for i, want := range test.want {
			b, err := r.Next()
			if err != nil {
				switch {
				case want == "" && errors.Is(err, CorruptRecord):
					// do nothing, this is fine
				case want == "" && !errors.Is(err, CorruptRecord):
					t.Fatalf("Next_corrupt %q i=%d: expected corruption error, got %v with value %q", test.name, i, err, string(b))
				default:
					t.Fatalf("Next_corrupt %q i=%d: %v", test.name, i, err)
				}
			}
			if diff := cmp.Diff(string(b), want); diff != "" {
				t.Errorf("Next_corrupt %q: unexpected diff in record %d:\n%v", test.name, i, diff)
			}
		}
	}
}

package queues

import (
	"log"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNamespace(t *testing.T) {
	cases := []struct {
		name  string
		queue string
		ok    bool
		want  string
	}{
		{
			name:  "no ns",
			queue: "/some/path/somewhere",
		},
		{
			name:  "not a path",
			queue: "this is not a path",
		},
		{
			name:  "simple ns",
			queue: "/ns=something/and/other/stuff",
			ok:    true,
			want:  "something",
		},
		{
			name:  "double ns",
			queue: "/ns=something/and/ns=other/",
			ok:    true,
			want:  "something",
		},
		{
			name:  "no leading slash",
			queue: "ns=myns/not/really",
		},
		{
			name:  "not first component",
			queue: "/hey/ns=something/there",
		},
		{
			name:  "escaped slash",
			queue: "/ns=some\\/thing/hey",
			ok:    true,
			want:  "some/thing",
		},
	}

	for _, test := range cases {
		ns, ok := Namespace(test.queue)
		if test.ok != ok {
			t.Errorf("TestNamespace %q: wanted ok=%v, got %v", test.name, test.ok, ok)
		}
		if test.want != ns {
			t.Errorf("TestNamespace %q: wanted ns=%v, got %v", test.name, test.want, ns)
		}
	}
}

func TestPathParams(t *testing.T) {
	cases := []struct {
		name  string
		queue string
		want  map[string][]string
	}{
		{
			name:  "no params",
			queue: "/some/path/somewhere",
		},
		{
			name:  "leading param",
			queue: "/key=someval/hello",
			want: map[string][]string{
				"key": []string{"someval"},
			},
		},
		{
			name:  "multiple same",
			queue: "/hey there/key=someval/and stuff/key=otherval",
			want: map[string][]string{
				"key": []string{"someval", "otherval"},
			},
		},
		{
			name:  "escaped key",
			queue: "/n\\/s=something/hey",
			want: map[string][]string{
				"n/s": []string{"something"},
			},
		},
		{
			name:  "escaped value",
			queue: "/key=val\\/ue/hey",
			want: map[string][]string{
				"key": []string{"val/ue"},
			},
		},
		{
			name:  "terminal value, no slash",
			queue: "/hey there/key=value",
			want: map[string][]string{
				"key": []string{"value"},
			},
		},
		{
			name:  "repeated terminal",
			queue: "/hey/k=something/there/k=else",
			want: map[string][]string{
				"k": []string{"something", "else"},
			},
		},
		{
			name:  "leading without slash prefix",
			queue: "key=something/and/other/stuff",
		},
	}

	for _, test := range cases {
		params := PathParams(test.queue)
		if diff := cmp.Diff(test.want, params); diff != "" {
			log.Printf("want %v, got %v", test.want, params)
			t.Errorf("TestPathParams %q (-want +got):\n%v", test.name, diff)
		}
	}
}

func TestPathComponents(t *testing.T) {
	cases := []struct {
		name  string
		queue string
		want  []string
	}{
		{
			name:  "empty",
			queue: "",
		},
		{
			name:  "no slashes",
			queue: "hey there",
			want:  []string{"hey there"},
		},
		{
			name:  "one with prefix",
			queue: "/hey there",
			want:  []string{"/hey there"},
		},
		{
			name:  "trailing slash",
			queue: "/hey there/and stuff/",
			want:  []string{"/hey there", "/and stuff", "/"},
		},
		{
			name:  "escaping",
			queue: "/hey\\/there/and\\/stuff\\//",
			want:  []string{"/hey/there", "/and/stuff/", "/"},
		},
	}

	for _, test := range cases {
		if diff := cmp.Diff(test.want, PathComponents(test.queue)); diff != "" {
			t.Errorf("TestPathComponents %q (-want +got):\n%v", test.name, diff)
		}
	}
}

func TestEscapeComponent(t *testing.T) {
	cases := []struct {
		name      string
		component string
		want      string
	}{
		{
			name:      "nothing to escape",
			component: "lots of stuff, none of it weird",
			want:      "lots of stuff, none of it weird",
		},
		{
			name: "empty",
		},
		{
			name:      "forward slashes",
			component: "something/in/here/needs/escaping/",
			want:      "something\\/in\\/here\\/needs\\/escaping\\/",
		},
		{
			name:      "backslash",
			component: "something\\is\\strange\\here",
			want:      "something\\\\is\\\\strange\\\\here",
		},
		{
			name:      "consecutive backslashes and slashes",
			component: "a/\\\\//",
			want:      "a\\/\\\\\\\\\\/\\/",
		},
		{
			name:      "trailing backslash",
			component: "a\\",
			want:      "a\\\\",
		},
	}

	for _, test := range cases {
		if got, want := EscapeComponent(test.component), test.want; got != want {
			t.Errorf("TestEscapeComponent %q: expected %q, got %q", test.name, want, got)
		}
	}
}

func TestTryAddNamespace(t *testing.T) {
	cases := []struct {
		name      string
		queue     string
		namespace string
		want      string
	}{
		{
			name:      "empty namespace",
			queue:     "no matter",
			namespace: "",
			want:      "/ns=/no matter",
		},
		{
			name:      "already namespaced",
			queue:     "/ns=something/other stuff",
			namespace: "other",
			want:      "/ns=something/other stuff",
		},
		{
			name:      "invalid namespace exists",
			queue:     "ns=hey/there",
			namespace: "namespace",
			want:      "/ns=namespace/ns=hey/there",
		},
		{
			name:      "no namespace yet",
			queue:     "/this/is/a/path",
			namespace: "namesy",
			want:      "/ns=namesy/this/is/a/path",
		},
	}

	for _, test := range cases {
		if got, want := TryAddNamespace(test.queue, test.namespace), test.want; got != want {
			t.Errorf("TestTryAddNamespace %q: expected %q, got %q", test.name, want, got)
		}
	}
}

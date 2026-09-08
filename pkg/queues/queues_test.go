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
		{
			name:  "escaped semicolon",
			queue: "/ns=some\\;thing/hey",
			ok:    true,
			want:  "some;thing",
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
			name:  "compound params",
			queue: "/tasks/sess=abc;gc=123/inbox",
			want: map[string][]string{
				"gc":   []string{"123"},
				"sess": []string{"abc"},
			},
		},
		{
			name:  "compound params reverse order",
			queue: "/tasks/gc=123;sess=abc/inbox",
			want: map[string][]string{
				"gc":   []string{"123"},
				"sess": []string{"abc"},
			},
		},
		{
			name:  "repeated compound params preserve order",
			queue: "/gc=100;sess=abc/sub/sess=def;gc=200",
			want: map[string][]string{
				"gc":   []string{"100", "200"},
				"sess": []string{"abc", "def"},
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
			name:  "escaped semicolon",
			queue: "/key=val\\;ue;gc=123/hey",
			want: map[string][]string{
				"gc":  []string{"123"},
				"key": []string{"val;ue"},
			},
		},
		{
			name:  "equals in value",
			queue: "/key=left=right;other=value",
			want: map[string][]string{
				"key":   []string{"left=right"},
				"other": []string{"value"},
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
		{
			name:  "escaped leading slash",
			queue: "\\/key=something/and/other/stuff",
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

func TestFoldPathParam(t *testing.T) {
	cases := []struct {
		name string
		path string
		key  string
		want string
	}{
		{
			name: "no matching parameter",
			path: "/foo/bar/gc=123/baz",
			key:  "sess",
			want: "/foo/bar/gc=123/baz",
		},
		{
			name: "standalone parameter",
			path: "/foo/bar/sess=123/baz",
			key:  "sess",
			want: "/foo/bar/*/baz",
		},
		{
			name: "compound parameter first",
			path: "/foo/bar/sess=123;gc=456/baz",
			key:  "sess",
			want: "/foo/bar/*/baz",
		},
		{
			name: "compound parameter last",
			path: "/foo/bar/gc=456;sess=123/baz",
			key:  "sess",
			want: "/foo/bar/*/baz",
		},
		{
			name: "empty parameter",
			path: "/foo/sess=/bar",
			key:  "sess",
			want: "/foo/*/bar",
		},
		{
			name: "multiple matching components",
			path: "/sess=one/foo/gc=123;sess=two/bar",
			key:  "sess",
			want: "/*/foo/*/bar",
		},
		{
			name: "escaped semicolon is literal",
			path: `/foo/name=value\;sess=123/bar`,
			key:  "sess",
			want: `/foo/name=value\;sess=123/bar`,
		},
		{
			name: "preserve unrelated escapes",
			path: `/foo\/bar/sess=123/baz\\quux`,
			key:  "sess",
			want: `/foo\/bar/*/baz\\quux`,
		},
		{
			name: "relative leading parameter is literal",
			path: "sess=123/foo/bar",
			key:  "sess",
			want: "sess=123/foo/bar",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := FoldPathParam(test.path, test.key); got != test.want {
				t.Errorf("FoldPathParam(%q, %q) = %q, want %q", test.path, test.key, got, test.want)
			}
		})
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
			name:      "semicolons",
			component: "something;needs;escaping",
			want:      "something\\;needs\\;escaping",
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

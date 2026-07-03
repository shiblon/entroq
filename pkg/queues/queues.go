// Package queues contains helper functions for manipulation of queue names.
package queues

import (
	"path"
	"strings"
)

// EscapeComponent adds backslash in front of backslash and forward slash.
func EscapeComponent(c string) string {
	replacer := strings.NewReplacer("/", "\\/", "\\", "\\\\")
	return replacer.Replace(c)
}

// PathComponents returns a list of the path components in this queue
// name. It basically assumes that the name is structured like a POSIX file
// path, including escaping of / where needed. It includes the beginning '/' as
// part of the component, if it has one. A path ending in / will have a final
// component of just that character.
func PathComponents(qname string) []string {
	var components []string
	buf := ""
	isEsc := false
	for _, c := range qname {
		if !isEsc && c == '/' && buf != "" {
			// Time to ship out a component, reset the buffer.
			components = append(components, buf)
			buf = ""
		}
		if c != '\\' || isEsc {
			buf += string(c)
		}
		isEsc = c == '\\' && !isEsc
	}
	if buf != "" {
		components = append(components, buf)
	}
	return components
}

// PathLabels returns up to three cumulative path labels for a queue name,
// suitable for grouping metrics by hierarchy: for "/a/b/c/d" it returns "/a",
// "/a/b", and "/a/b/c". Components beyond the third are ignored, and unused
// levels are empty. It uses the same escape-aware splitting as PathComponents,
// so an escaped slash within a component does not begin a new level.
func PathLabels(qname string) (l1, l2, l3 string) {
	c := PathComponents(qname)
	if len(c) > 0 {
		l1 = c[0]
	}
	if len(c) > 1 {
		l2 = l1 + c[1]
	}
	if len(c) > 2 {
		l3 = l2 + c[2]
	}
	return l1, l2, l3
}

// PathParams returns a map from strings to slices of values. It looks
// through the queue name, assuing that it is basically structured like a path,
// with some `/key=value/` components, and extracts those key/value pairs into
// what is essentially a multimap.
//
// Note that `/key=` is the criterion for search. If the key=value does not
// start with a slash, it is not picked up. This allows for escaping of the /
// character, so `\/` cannot start one of these path components.
func PathParams(qname string) map[string][]string {
	params := make(map[string][]string)
	for _, component := range PathComponents(qname) {
		if !strings.HasPrefix(component, "/") {
			continue
		}
		// Remove the prefix, which we know uses 1 byte, now:
		keyVal := component[1:]

		key, val, found := strings.Cut(keyVal, "=")
		if !found {
			continue
		}
		params[key] = append(params[key], val)
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

// Namespace returns the namespace component from a queue name, if it exists.
func Namespace(qname string) (string, bool) {
	if qname == "" {
		return "", false
	}
	if !strings.HasPrefix(qname, "/ns=") {
		return "", false
	}
	// Starts with /ns=, so get the next component from that.
	params := PathParams(qname)
	if ns, ok := params["ns"]; ok {
		return ns[0], true // only the first one matters.
	}
	return "", false
}

// TryAddNamespace attempts to add the given namespace to the queue name. If
// there is already a namespace, it gives up. Otherwise it prepends with /ns=namespace/.
func TryAddNamespace(qname, ns string) string {
	if _, ok := Namespace(qname); ok {
		return qname
	}
	return path.Join("/ns="+EscapeComponent(ns), qname)
}

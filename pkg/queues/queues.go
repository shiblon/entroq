// Package queues contains helper functions for manipulation of queue names.
package queues

import (
	"path"
	"strings"
)

// EscapeComponent adds a backslash in front of characters that are structural
// within a path component.
func EscapeComponent(c string) string {
	replacer := strings.NewReplacer("/", "\\/", ";", "\\;", "\\", "\\\\")
	return replacer.Replace(c)
}

// PathComponents returns a list of the path components in this queue
// name. It basically assumes that the name is structured like a POSIX file
// path, including escaping of / where needed. It includes the beginning '/' as
// part of the component, if it has one. A path ending in / will have a final
// component of just that character.
func PathComponents(qname string) []string {
	if qname == "" {
		return nil
	}

	parts := splitUnescaped(qname, '/')
	components := make([]string, 0, len(parts))
	if parts[0] != "" {
		components = append(components, unescape(parts[0]))
	}
	for _, part := range parts[1:] {
		components = append(components, "/"+unescape(part))
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
// through the queue name, assuming that it is basically structured like a path,
// with some `/key=value/` components, and extracts those key/value pairs into
// what is essentially a multimap. A component may contain several pairs
// separated by semicolons, as in `/key=value;other=value/`. An escaped semicolon
// is part of the key or value rather than a separator.
//
// Only components introduced by an unescaped slash are inspected. The first
// pair begins immediately after that slash and later pairs begin after an
// unescaped semicolon. Thus an escaped leading slash cannot introduce params.
func PathParams(qname string) map[string][]string {
	params := make(map[string][]string)
	components := splitUnescaped(qname, '/')
	for _, component := range components[1:] {
		visitComponentParams(component, func(key, val string) bool {
			params[key] = append(params[key], val)
			return true
		})
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

// FoldPathParam replaces every slash-prefixed path component containing key
// with "*". Components without the parameter are preserved byte-for-byte,
// including their escapes. This is useful when a policy marker identifies a
// high-cardinality unit whose individual names should be aggregated.
func FoldPathParam(path, key string) string {
	components := splitUnescaped(path, '/')
	for i := 1; i < len(components); i++ {
		visitComponentParams(components[i], func(paramKey, _ string) bool {
			if paramKey != key {
				return true
			}
			components[i] = "*"
			return false
		})
	}
	return strings.Join(components, "/")
}

// visitComponentParams visits the ordered parameters in one raw path
// component. Returning false stops iteration.
func visitComponentParams(component string, visit func(key, val string) bool) {
	for _, rawKeyVal := range splitUnescaped(component, ';') {
		keyVal := unescape(rawKeyVal)
		key, val, found := strings.Cut(keyVal, "=")
		if found && !visit(key, val) {
			return
		}
	}
}

// splitUnescaped splits s at separator runes that are not backslash-escaped.
// It retains escapes so callers can perform nested splitting before unescaping.
func splitUnescaped(s string, separator rune) []string {
	parts := make([]string, 0, strings.Count(s, string(separator))+1)
	var part strings.Builder
	escaped := false
	for _, r := range s {
		if r == separator && !escaped {
			parts = append(parts, part.String())
			part.Reset()
			continue
		}
		part.WriteRune(r)
		if r == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
	}
	return append(parts, part.String())
}

// unescape removes the backslash that quotes the following rune. A trailing
// unmatched backslash is discarded, preserving PathComponents' historic
// behavior.
func unescape(s string) string {
	var value strings.Builder
	escaped := false
	for _, r := range s {
		if escaped {
			value.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		value.WriteRune(r)
	}
	return value.String()
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

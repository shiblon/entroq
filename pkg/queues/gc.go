package queues

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseGCActivation interprets a garbage-collection activation value: the time
// after which a queue's claimable tasks may be collected.
//
// The empty string means "always active" and yields the zero Time. A value
// consisting only of decimal digits is a Unix timestamp in SECONDS, so "0" is
// the epoch (also effectively always active). Millisecond timestamps are not
// detected, so millisecond-based clients such as JavaScript must divide by
// 1000. Any other value is parsed as RFC3339Nano, which accepts both plain and
// fractional-second forms (including the output of JavaScript's
// Date.toISOString).
//
// Per-task arrival time still governs when an individual task is collected;
// this value is only the whole-queue on-switch.
func ParseGCActivation(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if isAllDigits(value) {
		secs, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("gc activation unix seconds %q: %w", value, err)
		}
		return time.Unix(secs, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("gc activation timestamp %q: %w", value, err)
	}
	return t, nil
}

// GCActivation resolves a queue's garbage-collection policy from its /gc= path
// components. present is false when no /gc= component appears. When several are
// present the most specific one wins: the last in path order. A malformed value
// yields an error, and the caller must not collect the queue on error.
func GCActivation(qname string) (activateAt time.Time, present bool, err error) {
	last := ""
	for _, component := range PathComponents(qname) {
		if !strings.HasPrefix(component, "/") {
			continue
		}
		key, val, found := strings.Cut(component[1:], "=")
		if !found || key != "gc" {
			continue
		}
		last, present = val, true
	}
	if !present {
		return time.Time{}, false, nil
	}
	at, err := ParseGCActivation(last)
	return at, true, err
}

// isAllDigits reports whether s is non-empty and composed solely of the ASCII
// digits 0-9 (no sign, decimal point, or whitespace).
func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

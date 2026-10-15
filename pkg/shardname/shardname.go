// Package shardname names the files of a sharded set: base-00003-of-00008 is
// shard 3 of 8, and the spec base@8 names the whole set.
package shardname

import (
	"fmt"
	"strconv"
	"strings"
)

// minDigits is the narrowest a shard number is written.
const minDigits = 5

// Name returns the name of shard i of n.
func Name(base string, i, n int) string {
	w := max(minDigits, len(strconv.Itoa(n)))
	return fmt.Sprintf("%s-%0*d-of-%0*d", base, w, i, w, n)
}

// Expand returns the names of every shard of spec, in order. A spec is
// base@N, for N >= 1.
func Expand(spec string) ([]string, error) {
	at := strings.LastIndexByte(spec, '@')
	if at < 0 {
		return nil, fmt.Errorf("shard spec %q has no @N", spec)
	}
	base, count := spec[:at], spec[at+1:]
	if base == "" {
		return nil, fmt.Errorf("shard spec %q has no base name", spec)
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 || count != strconv.Itoa(n) {
		return nil, fmt.Errorf("shard spec %q: shard count must be a positive integer", spec)
	}
	names := make([]string, n)
	for i := range n {
		names[i] = Name(base, i, n)
	}
	return names, nil
}

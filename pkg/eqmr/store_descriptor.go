package eqmr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"unicode/utf8"
)

// maxDescriptorFieldBytes bounds each descriptor field. Descriptors are stamped
// on tasks, run pointers, and run state, so they stay small.
const maxDescriptorFieldBytes = 256

// Descriptor durably names a store or sink without saying how to reach it.
// Name is the logical name a process resolves through its own configuration;
// Driver is the versioned implementation and Identity the
// credential-independent instance, both as the implementation reports them.
//
// A descriptor never carries a command, address, or credential: those are
// operator-local grants, and durable state must not be able to select them.
type Descriptor struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Identity string `json:"identity"`
}

// fitText shortens s to at most max bytes while keeping it readable at a
// glance: a long s becomes its head, a short hash of the whole of it, and its
// tail, cut on rune boundaries. Two long strings that differ anywhere differ in
// the hash, so shortening never makes distinct names collide in practice.
func fitText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	mid := "~" + hex.EncodeToString(sum[:8]) + "~"
	room := max - len(mid)
	if room <= 0 {
		return mid[:max]
	}
	head := room / 2
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (room - head)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + mid + s[tail:]
}

func (d Descriptor) String() string {
	return fmt.Sprintf("%s (driver %s, identity %s)", d.Name, d.Driver, d.Identity)
}

func (d Descriptor) validate() error {
	for _, f := range []struct{ what, val string }{
		{"name", d.Name},
		{"driver", d.Driver},
		{"identity", d.Identity},
	} {
		if f.val == "" {
			return fmt.Errorf("descriptor %s is required", f.what)
		}
		if err := ValidText("descriptor "+f.what, f.val, maxDescriptorFieldBytes); err != nil {
			return err
		}
	}
	return nil
}

// registry is a process's view of the things of one kind it can reach, such as
// intermediate stores or sinks, keyed by logical name. Durable state names one
// by descriptor; resolving it here either finds exactly that one or fails, so
// nothing is ever read from or written to a different one that happens to
// share its name.
type registry[T any] struct {
	what   string
	byName map[string]registered[T]
}

type registered[T any] struct {
	desc Descriptor
	item T
}

// newRegistry indexes items by the descriptor describe reports for each. what
// names the kind in errors.
func newRegistry[T any](what string, describe func(T) Descriptor, items ...T) (*registry[T], error) {
	r := &registry[T]{what: what, byName: make(map[string]registered[T], len(items))}
	for _, item := range items {
		d := describe(item)
		if err := d.validate(); err != nil {
			return nil, fmt.Errorf("eqmr %s: %w", what, err)
		}
		if _, ok := r.byName[d.Name]; ok {
			return nil, fmt.Errorf("eqmr %s %q: configured twice", what, d.Name)
		}
		r.byName[d.Name] = registered[T]{desc: d, item: item}
	}
	return r, nil
}

func (r *registry[T]) resolve(d Descriptor) (T, error) {
	var zero T
	reg, ok := r.byName[d.Name]
	if !ok {
		return zero, fmt.Errorf("%s %s is not configured in this process", r.what, d)
	}
	if reg.desc != d {
		return zero, fmt.Errorf("%s %q is %s here, but the run names %s", r.what, d.Name, reg.desc, d)
	}
	return reg.item, nil
}

// intermediateStores is the registry of intermediate stores.
type intermediateStores = *registry[IntermediateStore]

func newIntermediateStores(stores ...IntermediateStore) (intermediateStores, error) {
	return newRegistry("intermediate store", IntermediateStore.descriptor, stores...)
}

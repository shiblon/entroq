package eqmr

import (
	"fmt"
)

// maxStoreDescriptorFieldBytes bounds each descriptor field. Descriptors are
// stamped on every map task and run pointer, so they stay small.
const maxStoreDescriptorFieldBytes = 256

// storeDescriptor durably names an intermediate store without saying how to
// reach it. Name is the logical store a process resolves through its own
// configuration; Driver is the versioned implementation and Identity the
// credential-independent store instance, both as the store reports them.
//
// A descriptor never carries a command, address, or credential: those are
// operator-local grants, and durable state must not be able to select them.
type storeDescriptor struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Identity string `json:"identity"`
}

func (d storeDescriptor) String() string {
	return fmt.Sprintf("%s (driver %s, identity %s)", d.Name, d.Driver, d.Identity)
}

func (d storeDescriptor) validate() error {
	for _, f := range []struct{ what, val string }{
		{"store name", d.Name},
		{"store driver", d.Driver},
		{"store identity", d.Identity},
	} {
		if f.val == "" {
			return fmt.Errorf("%s is required", f.what)
		}
		if err := ValidText(f.what, f.val, maxStoreDescriptorFieldBytes); err != nil {
			return err
		}
	}
	return nil
}

// intermediateStores is a process's view of the stores it can reach, keyed by
// logical name. Durable state names a store by descriptor; resolving it here
// either finds exactly that store or fails, so a run is never read from or
// written to a different store that happens to share its name.
type intermediateStores map[string]intermediateStore

func newIntermediateStores(stores ...intermediateStore) (intermediateStores, error) {
	r := make(intermediateStores, len(stores))
	for _, s := range stores {
		d := s.descriptor()
		if err := d.validate(); err != nil {
			return nil, fmt.Errorf("eqmr intermediate store: %w", err)
		}
		if _, ok := r[d.Name]; ok {
			return nil, fmt.Errorf("eqmr intermediate store %q: configured twice", d.Name)
		}
		r[d.Name] = s
	}
	return r, nil
}

func (r intermediateStores) resolve(d storeDescriptor) (intermediateStore, error) {
	s, ok := r[d.Name]
	if !ok {
		return nil, fmt.Errorf("intermediate store %s is not configured in this process", d)
	}
	if have := s.descriptor(); have != d {
		return nil, fmt.Errorf("intermediate store %q is %s here, but the run names %s", d.Name, have, d)
	}
	return s, nil
}

package eqmr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

func TestStoreDescriptorValidate(t *testing.T) {
	good := storeDescriptor{Name: "scratch", Driver: "fs/1", Identity: "host-a:/var/eqmr"}
	if err := good.validate(); err != nil {
		t.Fatalf("valid descriptor rejected: %v", err)
	}
	cases := map[string]storeDescriptor{
		"no name":     {Driver: "fs/1", Identity: "x"},
		"no driver":   {Name: "scratch", Identity: "x"},
		"no identity": {Name: "scratch", Driver: "fs/1"},
		"NUL":         {Name: "scr\x00atch", Driver: "fs/1", Identity: "x"},
		"invalid":     {Name: "scratch", Driver: "fs/1", Identity: "\xff"},
		"too long":    {Name: "scratch", Driver: "fs/1", Identity: strings.Repeat("x", maxStoreDescriptorFieldBytes+1)},
	}
	for name, d := range cases {
		if err := d.validate(); err == nil {
			t.Errorf("%s: descriptor %+v accepted", name, d)
		}
	}
}

// describedStore is a testRunStore with a chosen descriptor.
type describedStore struct {
	testRunStore
	desc storeDescriptor
}

func (s *describedStore) descriptor() storeDescriptor { return s.desc }

func TestIntermediateStoresResolve(t *testing.T) {
	here := &describedStore{desc: storeDescriptor{Name: "scratch", Driver: "fs/1", Identity: "host-a"}}
	stores, err := newIntermediateStores(here)
	if err != nil {
		t.Fatalf("new stores: %v", err)
	}

	got, err := stores.resolve(here.desc)
	if err != nil || got != here {
		t.Fatalf("resolve exact descriptor: got %v, %v", got, err)
	}
	for name, d := range map[string]storeDescriptor{
		"unknown name":   {Name: "other", Driver: "fs/1", Identity: "host-a"},
		"other identity": {Name: "scratch", Driver: "fs/1", Identity: "host-b"},
		"other driver":   {Name: "scratch", Driver: "fs/2", Identity: "host-a"},
	} {
		if _, err := stores.resolve(d); err == nil {
			t.Errorf("%s: resolved %v", name, d)
		}
	}

	if _, err := newIntermediateStores(here, &describedStore{desc: here.desc}); err == nil {
		t.Error("duplicate store name accepted")
	}
	if _, err := newIntermediateStores(&describedStore{desc: storeDescriptor{Name: "bare"}}); err == nil {
		t.Error("invalid store descriptor accepted")
	}
}

func TestOpenMergedRejectsForeignStore(t *testing.T) {
	ctx := context.Background()
	store := &describedStore{desc: storeDescriptor{Name: "scratch", Driver: "fs/1", Identity: "host-a"}}
	ref, err := store.put(ctx, []intermediateRecord{{Primary: "k", Value: "v"}})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	stores, err := newIntermediateStores(store)
	if err != nil {
		t.Fatalf("new stores: %v", err)
	}
	foreign := store.desc
	foreign.Identity = "host-b"
	_, err = openMergedIntermediate(ctx, stores, []intermediateRun{{Store: foreign, Ref: ref}})
	if err == nil || !strings.Contains(err.Error(), "host-b") {
		t.Fatalf("open run from a same-named store elsewhere: got %v, want identity mismatch", err)
	}
}

func newDescriptorTestRun(ctx context.Context, t *testing.T) (*entroq.EntroQ, *Controller) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open in-memory client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	ctrl, err := New(eq, "/eqmrtest/"+entroq.GenHex16(), WithMapShards(1), WithReduceShards(1))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := ctrl.Setup(ctx, []*KV{NewKV("", "a b a")}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return eq, ctrl
}

// rewriteMapTask replaces the run's single map task with one whose store is
// changed by edit, after checking that Setup stamped the run's own store.
func rewriteMapTask(ctx context.Context, t *testing.T, eq *entroq.EntroQ, ctrl *Controller, edit func(*docRef)) {
	t.Helper()
	tasks, err := eq.Tasks(ctx, ctrl.MapQ())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("list map tasks: got %d, %v", len(tasks), err)
	}
	var ref docRef
	if err := json.Unmarshal(tasks[0].Value, &ref); err != nil {
		t.Fatalf("decode map task: %v", err)
	}
	if want := ctrl.documentStore().descriptor(); ref.Store != want {
		t.Fatalf("setup stamped store %v, want %v", ref.Store, want)
	}
	edit(&ref)
	if _, err := eq.Modify(ctx, tasks[0].Delete(), entroq.InsertingInto(ctrl.MapQ(), entroq.WithValue(ref))); err != nil {
		t.Fatalf("rewrite map task: %v", err)
	}
}

func runOneMapper(ctx context.Context, ctrl *Controller) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ctrl.MapperWorker(WordCountMapper).Run(ctx,
		worker.Watching(ctrl.MapQ()), worker.WithLease(10*time.Second))
}

func TestMapperExitsOnUnreachableStore(t *testing.T) {
	ctx := context.Background()
	eq, ctrl := newDescriptorTestRun(ctx, t)
	rewriteMapTask(ctx, t, eq, ctrl, func(ref *docRef) { ref.Store.Identity = "/some/other/run" })

	err := runOneMapper(ctx, ctrl)
	if err == nil || !strings.Contains(err.Error(), "/some/other/run") {
		t.Fatalf("mapper on an unreachable store: got %v, want an exit naming the store", err)
	}
	// The task is left for a worker that can reach the store, not quarantined.
	if quarantined, err := eq.Tasks(ctx, ctrl.ErrQ()); err != nil || len(quarantined) != 0 {
		t.Fatalf("error queue: got %d tasks, %v; want none", len(quarantined), err)
	}
	if left, err := eq.Tasks(ctx, ctrl.MapQ()); err != nil || len(left) != 1 {
		t.Fatalf("map queue: got %d tasks, %v; want the task left in place", len(left), err)
	}
}

func TestMapperQuarantinesTaskWithoutStore(t *testing.T) {
	ctx := context.Background()
	eq, ctrl := newDescriptorTestRun(ctx, t)
	rewriteMapTask(ctx, t, eq, ctrl, func(ref *docRef) { ref.Store = storeDescriptor{} })

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runOneMapper(runCtx, ctrl) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		quarantined, err := eq.Tasks(ctx, ctrl.ErrQ())
		if err != nil {
			t.Fatalf("list error queue: %v", err)
		}
		if len(quarantined) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("map task with no store was not quarantined")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && runCtx.Err() == nil {
		t.Fatalf("mapper: %v", err)
	}
}

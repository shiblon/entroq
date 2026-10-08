package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestRunsOnOneClientExcludeEachOther pins that two runs sharing a client
// exclude each other on a doc they both take, because each runs under its own
// claimant by default. A claim excludes other claimants only, so with a shared
// claimant (WithClaimant("")) both runs would hold the doc at once.
//
// It also exercises every operation the run performs as its claimant: the doc
// is renewed mid-work (the handler outlasts half the lease) and changed in the
// commit, and either would fail under the wrong claimant.
func TestRunsOnOneClientExcludeEachOther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const (
		tasks = 6
		lease = 200 * time.Millisecond
	)
	args := []entroq.ModifyArg{entroq.PuttingDocInto("ns", entroq.WithKeys("counter", ""), entroq.WithContent(0))}
	for range tasks {
		args = append(args, entroq.InsertingInto("q", entroq.WithValue("work")))
	}
	if _, err := client.Modify(ctx, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var holders, maxHolders atomic.Int32
	w := New(client,
		WithTakeDocs(func(context.Context, *entroq.Task, string) ([]*entroq.DocClaim, error) {
			return []*entroq.DocClaim{entroq.ClaimKey("ns", "counter")}, nil
		}),
		WithDoModify(func(_ context.Context, task *entroq.Task, _ string, docs []*entroq.Doc) (*Result, error) {
			n := holders.Add(1)
			defer holders.Add(-1)
			for {
				m := maxHolders.Load()
				if n <= m || maxHolders.CompareAndSwap(m, n) {
					break
				}
			}
			count, err := entroq.GetContent[int](docs[0])
			if err != nil {
				return nil, FatalErrorf("parse counter: %v", err)
			}
			time.Sleep(lease * 3 / 4)
			return Modify(task.Delete(), docs[0].Change(entroq.WithContent(count+1))), nil
		}),
	)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Run(runCtx, Watching("q"), WithLease(lease), WithBaseRetryDelay(0))
		}()
	}

	for {
		docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: "ns", KeyExact: "counter"})
		if err != nil {
			t.Fatalf("read counter: %v", err)
		}
		count, err := entroq.GetContent[int](docs[0])
		if err != nil {
			t.Fatalf("parse counter: %v", err)
		}
		if count == tasks {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("counter reached %d of %d", count, tasks)
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	wg.Wait()

	if got := maxHolders.Load(); got != 1 {
		t.Errorf("%d runs held the doc at once, want 1", got)
	}
}

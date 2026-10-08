package eqmr_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
)

// memSink is a Sink that keeps everything in memory and records what the run
// asked of it, so a test can check the contract from the sink's side.
type memSink struct {
	name string

	// failCommitOnce makes the first commit take effect and then report an
	// error, as a controller that commits and dies before recording it does.
	failCommitOnce bool

	mu        sync.Mutex
	nextID    int
	staged    map[string][]*eqmr.KV // staged attempts not yet swept
	committed map[int]string        // partition -> committed attempt
	swept     int
	finished  int
	parts     int
	empty     []int
}

func newMemSink(name string) *memSink {
	return &memSink{name: name, staged: make(map[string][]*eqmr.KV), committed: make(map[int]string)}
}

func (s *memSink) Descriptor() eqmr.Descriptor {
	return eqmr.Descriptor{Name: s.name, Driver: "test.mem/1", Identity: s.name}
}

func (s *memSink) Stage(context.Context, string, int) (eqmr.SinkWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return &memSinkWriter{sink: s, id: fmt.Sprintf("attempt-%d", s.nextID)}, nil
}

func (s *memSink) Commit(_ context.Context, _ string, partition int, staged json.RawMessage) error {
	var id string
	if err := json.Unmarshal(staged, &id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.staged[id]; !ok {
		return fmt.Errorf("commit of unknown attempt %q", id)
	}
	if _, ok := s.committed[partition]; !ok {
		s.committed[partition] = id
	}
	if s.failCommitOnce {
		s.failCommitOnce = false
		return fmt.Errorf("committed %q, then lost the reply", id)
	}
	return nil
}

func (s *memSink) Finish(_ context.Context, _ string, partitions int, empty []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	s.parts, s.empty = partitions, empty
	return nil
}

func (s *memSink) Sweep(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make(map[string]bool, len(s.committed))
	for _, id := range s.committed {
		keep[id] = true
	}
	for id := range s.staged {
		if !keep[id] {
			delete(s.staged, id)
			s.swept++
		}
	}
	return nil
}

// output merges the committed partitions, sorted by key.
func (s *memSink) output() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, id := range s.committed {
		for _, kv := range s.staged[id] {
			out = append(out, kv.String())
		}
	}
	slices.Sort(out)
	return out
}

type memSinkWriter struct {
	sink *memSink
	id   string
	kvs  []*eqmr.KV
}

func (w *memSinkWriter) Write(_ context.Context, key, value string) error {
	w.kvs = append(w.kvs, eqmr.NewKV(key, value))
	return nil
}

func (w *memSinkWriter) Close(context.Context) (json.RawMessage, error) {
	w.sink.mu.Lock()
	w.sink.staged[w.id] = w.kvs
	w.sink.mu.Unlock()
	return json.Marshal(w.id)
}

func TestCustomSinkReceivesCommittedOutput(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	sink := newMemSink("mem")
	const reduceShards = 6
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(3, reduceShards), eqmr.WithSink(sink))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	// Two distinct words, so most partitions receive nothing.
	input := []*eqmr.KV{eqmr.NewKV("", "alpha beta alpha"), eqmr.NewKV("", "beta")}
	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, want := strings.Join(sink.output(), ","), "(alpha)=2,(beta)=2"; got != want {
		t.Errorf("committed output %q, want %q", got, want)
	}
	sink.mu.Lock()
	finished, parts, empty := sink.finished, sink.parts, sink.empty
	committed := len(sink.committed)
	sink.mu.Unlock()
	if finished < 1 {
		t.Fatal("Finish was never called")
	}
	if parts != reduceShards {
		t.Errorf("Finish told %d partitions, want %d", parts, reduceShards)
	}
	if committed+len(empty) != reduceShards {
		t.Errorf("%d committed and %d empty partitions do not cover %d", committed, len(empty), reduceShards)
	}

	if _, err := ctrl.Results(ctx); err == nil || !strings.Contains(err.Error(), "read it with that sink") {
		t.Errorf("Results on a custom sink: got %v, want a refusal naming the sink", err)
	}
}

// TestCustomSinkSweepsLosingAttempts duplicates every reduce task, so every
// partition is staged twice. Exactly one attempt per partition is committed and
// every other attempt is swept once the run has ended.
func TestCustomSinkSweepsLosingAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	sink := newMemSink("mem")
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(4, 3), eqmr.WithSink(sink))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, histogram := wordCountFixture(t, 30, 300, 6)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = ctrl.RunController(runCtx) }()
	go func() { _ = ctrl.RunMapper(runCtx, eqmr.WordCountMapper) }()
	waitForPhase(ctx, t, ctrl, eqmr.PhaseReduce)

	partitions := duplicateTasks(ctx, t, eq, ctrl.ReduceQ())
	var reductions atomic.Int64
	for range 2 * partitions {
		go func() {
			_ = ctrl.RunReducer(runCtx, func(ctx context.Context, in eqmr.ReducerInput) (string, error) {
				reductions.Add(1)
				return eqmr.SumReducer(ctx, in)
			})
		}()
	}
	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	requireCollected(ctx, t, eq, ctrl)

	if got, want := strings.Join(sink.output(), ","), strings.Join(expectedFromHistogram(histogram), ","); got != want {
		t.Fatalf("committed output %v, want %v", got, want)
	}
	// Every attempt that was staged was either committed or swept.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.mu.Lock()
		leftover := len(sink.staged) - len(sink.committed)
		staged, swept := sink.nextID, sink.swept
		sink.mu.Unlock()
		if leftover == 0 {
			t.Logf("%d attempts staged, %d committed, %d swept", staged, partitions, swept)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d staged attempts neither committed nor swept", leftover)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEmptyMapOutputFinishesSink(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	sink := newMemSink("mem")
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(2, 4), eqmr.WithSink(sink))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := ctrl.Run(ctx, []*eqmr.KV{eqmr.NewKV("", " ")}, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 1, Reducers: 1,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.finished < 1 || len(sink.empty) != 4 || sink.nextID != 0 {
		t.Errorf("finished %d times with %v empty after %d stages; want finished with all 4 empty and nothing staged",
			sink.finished, sink.empty, sink.nextID)
	}
}

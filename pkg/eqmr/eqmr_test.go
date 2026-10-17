package eqmr_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/eqmr"
)

// testOpts is the standard option set for these tests: intervals tightened to
// the test's timescale.
func testOpts(mapShards, reduceShards int) []eqmr.Option {
	return []eqmr.Option{
		eqmr.WithMapShards(mapShards),
		eqmr.WithReduceShards(reduceShards),
		eqmr.WithLease(30 * time.Second),
	}
}

// testPrefix returns a fresh, unique run prefix.
func testPrefix() string { return "/eqmrtest/" + entroq.GenHex16() }

func newClient(ctx context.Context, t *testing.T) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
}

// uncollected lists the docs in a run's namespace that should not outlive it:
// anything but the state doc, result markers, and committed document-sink
// output. Staged output counts as committed only if a part doc references it,
// which is the stage-then-commit invariant itself.
func uncollected(ctx context.Context, t *testing.T, eq *entroq.EntroQ, ctrl *eqmr.Controller) []string {
	t.Helper()
	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: ctrl.DocNS()})
	if err != nil {
		t.Fatalf("list docs: %v", err)
	}
	committed := make(map[string]bool)
	for _, d := range docs {
		if !strings.HasPrefix(d.Key, "output/part/") {
			continue
		}
		var part struct {
			Chunks struct {
				Chunks []struct {
					ID string `json:"id"`
				} `json:"chunks"`
			} `json:"chunks"`
		}
		if err := json.Unmarshal(d.Content, &part); err != nil {
			t.Fatalf("parse part doc %q: %v", d.Key, err)
		}
		for _, c := range part.Chunks.Chunks {
			committed[c.ID] = true
		}
	}
	var leftover []string
	for _, d := range docs {
		switch {
		case d.Key == "state", d.Key == "output/manifest":
		case strings.HasPrefix(d.Key, "result/"), strings.HasPrefix(d.Key, "output/part/"):
		case strings.HasPrefix(d.Key, "output/staged/") && committed[d.ID]:
		default:
			leftover = append(leftover, d.Key)
		}
	}
	return leftover
}

// requireCollected waits until nothing in the run's namespace is uncollected.
func requireCollected(ctx context.Context, t *testing.T, eq *entroq.EntroQ, ctrl *eqmr.Controller) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		leftover := uncollected(ctx, t, eq, ctrl)
		if len(leftover) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d intermediate doc(s) never collected, e.g. %q", len(leftover), leftover[0])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mapOnly runs four mappers, and nothing else, until every split of a run that
// has been Setup has a finish task waiting. With no controller running, those
// runs are durable and unpublished, and still in place to be inspected.
func mapOnly(ctx context.Context, t *testing.T, eq *entroq.EntroQ, ctrl *eqmr.Controller, opts ...eqmr.MapperOption) {
	t.Helper()
	p, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	mapCtx, stopMappers := context.WithCancel(ctx)
	defer stopMappers()
	for range 4 {
		go func() { _ = ctrl.RunMapper(mapCtx, eqmr.WordCountMapper, opts...) }()
	}
	for {
		stats, err := eq.QueueStats(ctx, entroq.MatchExact(ctrl.MapDoneQ()))
		if err != nil {
			t.Fatalf("map-done queue stats: %v", err)
		}
		if s, ok := stats[ctrl.MapDoneQ()]; ok && s.Size >= p.Map.Total {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the map phase")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForPhase polls until the run reaches phase.
func waitForPhase(ctx context.Context, t *testing.T, ctrl *eqmr.Controller, phase string) {
	t.Helper()
	for {
		got, _, err := ctrl.Status(ctx)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if got == phase {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for phase %q", phase)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// runDirs lists the per-run directories under a file store root.
func runDirs(t *testing.T, root string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(root, "run-*"))
	if err != nil {
		t.Fatalf("list run directories: %v", err)
	}
	return dirs
}

// duplicateTasks inserts a copy of every task in queue, as a straggler policy
// would for a unit it considers late, and returns how many it copied.
func duplicateTasks(ctx context.Context, t *testing.T, eq *entroq.EntroQ, queue string) int {
	t.Helper()
	tasks, err := eq.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("list %s: %v", queue, err)
	}
	dupes := make([]entroq.ModifyArg, 0, len(tasks))
	for _, task := range tasks {
		dupes = append(dupes, entroq.InsertingInto(queue, entroq.WithRawValue(task.Value)))
	}
	if _, err := eq.Modify(ctx, dupes...); err != nil {
		t.Fatalf("duplicate %s: %v", queue, err)
	}
	return len(tasks)
}

func kvStrings(kvs []*eqmr.KV) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.String()
	}
	return out
}

func TestWordCountSmall(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(2, 3)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	// word1 appears 4 times, word3/word4/word5/word7 twice, the rest once.
	input := []*eqmr.KV{
		eqmr.NewKV("", ("word1 word2 word3 word4")),
		eqmr.NewKV("", ("word1 word3 word5 word7")),
		eqmr.NewKV("", ("word1 word4 word7 wordA")),
		eqmr.NewKV("", ("word1 word5 word9 wordE")),
	}

	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}

	want := []string{
		"(word1)=4", "(word2)=1", "(word3)=2", "(word4)=2", "(word5)=2",
		"(word7)=2", "(word9)=1", "(wordA)=1", "(wordE)=1",
	}
	got := kvStrings(results)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("results mismatch:\n got %v\nwant %v", got, want)
	}
}

// wordCountFixture builds documents over uniqueWords distinct words with a
// known histogram, so a test can assert the exact expected output.
func wordCountFixture(t *testing.T, uniqueWords, totalWords, numDocs int) ([]*eqmr.KV, map[string]int) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))

	histogram := make(map[string]int)
	occurrences := make([]string, 0, totalWords)
	for range totalWords {
		// Zero-padded so lexical and numeric order agree, making expected
		// output easy to construct.
		w := fmt.Sprintf("w%06d", rng.Intn(uniqueWords))
		histogram[w]++
		occurrences = append(occurrences, w)
	}
	rng.Shuffle(len(occurrences), func(i, j int) {
		occurrences[i], occurrences[j] = occurrences[j], occurrences[i]
	})

	perDoc := len(occurrences) / numDocs
	docs := make([]*eqmr.KV, 0, numDocs)
	for i := range numDocs {
		end := (i + 1) * perDoc
		if i == numDocs-1 {
			end = len(occurrences)
		}
		docs = append(docs, eqmr.NewKV("", (strings.Join(occurrences[i*perDoc:end], " "))))
	}
	return docs, histogram
}

func expectedFromHistogram(h map[string]int) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("(%s)=%d", k, h[k])
	}
	return out
}

// TestManyDistinctKeys is the case the older example never covered: enough
// distinct intermediate keys that they must actually be spread across reduce
// partitions. With one reduce task per key it would have created thousands of
// tasks; with a shuffle it creates exactly ReduceShards.
func TestManyDistinctKeys(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	const (
		uniqueWords = 5000
		totalWords  = 60000
		numDocs     = 40
	)
	input, histogram := wordCountFixture(t, uniqueWords, totalWords, numDocs)

	const reduceShards = 7
	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(8, reduceShards)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 4, Reducers: 3, Combiner: eqmr.SumCombiner,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}

	want := expectedFromHistogram(histogram)
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d distinct keys", len(results), len(want))
	}
	got := kvStrings(results)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("result %d: got %s, want %s", i, got[i], want[i])
		}
	}
	if !sort.SliceIsSorted(results, func(i, j int) bool {
		return results[i].Key < results[j].Key
	}) {
		t.Error("merged results are not sorted by key")
	}

	// The shuffle must actually spread keys, or the partition count is a lie.
	used := 0
	for p := range reduceShards {
		part, err := ctrl.ResultsForPartition(ctx, p)
		if err != nil {
			t.Fatalf("partition %d: %v", p, err)
		}
		if len(part) > 0 {
			used++
		}
		if !sort.SliceIsSorted(part, func(i, j int) bool {
			return part[i].Key < part[j].Key
		}) {
			t.Errorf("partition %d is not sorted by key", p)
		}
	}
	if used != reduceShards {
		t.Errorf("only %d of %d reduce partitions received keys", used, reduceShards)
	}
}

// TestCombinerDoesNotChangeResults pins the property that makes a Combiner an
// optimization rather than a semantic change.
func TestCombinerDoesNotChangeResults(t *testing.T) {
	ctx := context.Background()
	input, _ := wordCountFixture(t, 500, 8000, 12)

	run := func(t *testing.T, combiner eqmr.Combiner) []string {
		t.Helper()
		eq := newClient(ctx, t)
		ctrl, err := eqmr.New(eq, testPrefix(), testOpts(5, 4)...)
		if err != nil {
			t.Fatalf("new controller: %v", err)
		}
		if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
			Mappers: 3, Reducers: 2, Combiner: combiner,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		results, err := ctrl.Results(ctx)
		if err != nil {
			t.Fatalf("results: %v", err)
		}
		return kvStrings(results)
	}

	plain := run(t, nil)
	combined := run(t, eqmr.SumCombiner)

	if len(plain) != len(combined) {
		t.Fatalf("combiner changed result count: %d vs %d", len(plain), len(combined))
	}
	for i := range plain {
		if plain[i] != combined[i] {
			t.Fatalf("combiner changed result %d: %s vs %s", i, plain[i], combined[i])
		}
	}
}

// TestDifferentShardingSameResults pins that shard counts are a performance
// knob and never affect the answer.
func TestDifferentShardingSameResults(t *testing.T) {
	ctx := context.Background()
	input, _ := wordCountFixture(t, 300, 5000, 9)

	run := func(t *testing.T, mapShards, reduceShards int) []string {
		t.Helper()
		eq := newClient(ctx, t)
		ctrl, err := eqmr.New(eq, testPrefix(), testOpts(mapShards, reduceShards)...)
		if err != nil {
			t.Fatalf("new controller: %v", err)
		}
		if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
			Mappers: 2, Reducers: 2,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		results, err := ctrl.Results(ctx)
		if err != nil {
			t.Fatalf("results: %v", err)
		}
		return kvStrings(results)
	}

	a := run(t, 1, 1)
	b := run(t, 9, 13)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("sharding changed results:\n 1x1: %d keys\n 9x13: %d keys", len(a), len(b))
	}
}

// TestBinaryKeysAreRejected is the inverse of the guarantee this package used
// to make. Keys and values are text, and a job with genuinely binary data
// encodes it itself. What matters is that a violation fails loudly at the point
// it was produced, rather than being silently substituted with U+FFFD on the
// way through JSON or rejected much later by PostgreSQL's JSONB.
func TestBinaryKeysAreRejected(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{"invalid utf-8", string([]byte{0xff, 0xfe}), "not valid UTF-8"},
		{"unpaired surrogate bytes", string([]byte{0xed, 0xa0, 0x80}), "not valid UTF-8"},
		{"embedded NUL", string([]byte{'a', 0x00, 'b'}), "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eq := newClient(ctx, t)
			ctrl, err := eqmr.New(eq, testPrefix(), testOpts(1, 1)...)
			if err != nil {
				t.Fatalf("new controller: %v", err)
			}
			mapper := func(ctx context.Context, _, _ string, emit eqmr.EmitFunc) error {
				return emit(ctx, tc.key, "1")
			}
			err = ctrl.Run(ctx, []*eqmr.KV{eqmr.NewKV("", "anything")}, mapper, eqmr.SumReducer,
				eqmr.RunOptions{Mappers: 1, Reducers: 1})
			if err == nil {
				t.Fatal("expected the run to fail on a non-text key")
			}
			if !strings.Contains(err.Error(), "quarantined") {
				t.Errorf("error %q should report quarantined work", err)
			}
		})
	}
}

// TestValidateTextRule pins the rule itself, independently of a pipeline run.
func TestValidateTextRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantErr string
	}{
		{"plain ascii", "w000042", ""},
		{"multibyte utf-8", "\u65e5\u672c\u8a9e", ""},
		{"emoji", "\U0001f525", ""},
		{"empty", "", ""},
		{"invalid utf-8", string([]byte{0xff}), "not valid UTF-8"},
		{"NUL", string([]byte{'a', 0x00}), "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := eqmr.ValidateText("test value", tc.in)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestNewRejectsBadPrefix covers what New alone can check. Shard counts are
// deliberately absent here: a mapper or reducer pod builds a Controller without
// them, so requiring them at construction would break the very case the option
// form exists to serve.
func TestNewRejectsBadPrefix(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	for _, tc := range []struct {
		name   string
		prefix string
		want   string
	}{
		{"empty", "", "prefix is required"},
		{"too long", "/" + strings.Repeat("x", 1100), "limit is 1024"},
		{"multibyte over the byte limit", "/" + strings.Repeat("\u2192", 400), "limit is 1024"},
		{"invalid utf-8", string([]byte{'/', 0xff}), "not valid UTF-8"},
		{"embedded NUL", string([]byte{'/', 'a', 0x00}), "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := eqmr.New(eq, tc.prefix); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestWorkerRolesNeedNoShardCounts pins the property the whole option shape is
// for: a worker pod constructs its Controller from a prefix alone. Only Setup,
// which fixes the layout, requires the counts, and it says which one is missing.
func TestWorkerRolesNeedNoShardCounts(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix())
	if err != nil {
		t.Fatalf("a worker role must be able to build a Controller from a prefix alone: %v", err)
	}

	err = ctrl.Setup(ctx, []*eqmr.KV{eqmr.NewKV("", "a b c")})
	if err == nil {
		t.Fatal("Setup must refuse to run without shard counts")
	}
	if !strings.Contains(err.Error(), "MapShards must be set") {
		t.Errorf("error %q should name the missing option", err)
	}

	partial, err := eqmr.New(eq, testPrefix(), eqmr.WithMapShards(2))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	err = partial.Setup(ctx, []*eqmr.KV{eqmr.NewKV("", "a b c")})
	if err == nil || !strings.Contains(err.Error(), "ReduceShards must be set") {
		t.Errorf("error %q should name the missing option", err)
	}
}

// TestRunCollectsIntermediates pins that a completed run leaves nothing behind
// but its state and results: every intermediate run and pointer is deleted once
// its partition is reduced, and Run does not return before that happens.
func TestRunCollectsIntermediates(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(3, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, _ := wordCountFixture(t, 50, 500, 6)
	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, key := range uncollected(ctx, t, eq, ctrl) {
		t.Errorf("doc %q survived the run", key)
	}
}

func TestEmptyInputIsRejected(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(4, 3)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	err = ctrl.Run(ctx, nil, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "no input") {
		t.Fatalf("run with empty input: got %v, want a no-input error", err)
	}
	if _, _, err := ctrl.Status(ctx); err == nil {
		t.Error("empty input created a run")
	}
}

// TestEmptyMapOutputCompletes covers input that exists but maps to nothing:
// every split finishes with no runs, no partition has data, and the run is
// done without a reduce phase.
func TestEmptyMapOutputCompletes(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(4, 3)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input := []*eqmr.KV{eqmr.NewKV("", ""), eqmr.NewKV("", "   ")}
	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results, got %d", len(results))
	}
	p, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.Phase != eqmr.PhaseDone || p.Reduce.Done != p.Reduce.Total {
		t.Errorf("progress %+v, want done with every partition accounted for", p)
	}
}

// TestQuarantinedWorkFailsRun pins the failure story the Mapper contract
// describes: once real work has been quarantined, the controller fails the
// whole run with the task's own reason rather than waiting on it forever.
//
// It quarantines a map task directly rather than driving a poisonous record
// through repeated worker deaths. When a dead worker's task is reclaimed is the
// backend's business; noticing the result and ending the run is this package's.
func TestQuarantinedWorkFailsRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(2, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, _ := wordCountFixture(t, 10, 100, 4)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A map task that exhausted its claims, with its split still unprocessed,
	// lands in the map error queue looking like this.
	tasks, err := eq.Tasks(ctx, ctrl.MapQ())
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list map tasks: %d, %v", len(tasks), err)
	}
	if _, err := eq.Modify(ctx, tasks[0].Quarantine("maximum claims exceeded (10)", ctrl.MapQ()+"/err")); err != nil {
		t.Fatalf("quarantine map task: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() {
		if err := ctrl.RunController(runCtx); err != nil && runCtx.Err() == nil {
			t.Errorf("controller: %v", err)
		}
	}()

	err = ctrl.Wait(runCtx)
	if err == nil {
		t.Fatal("expected the run to fail with work in quarantine")
	}
	if !strings.Contains(err.Error(), "maximum claims exceeded") {
		t.Errorf("error %q does not carry the task's reason", err)
	}

	phase, reason, serr := ctrl.Status(ctx)
	if serr != nil {
		t.Fatalf("status: %v", serr)
	}
	if phase != eqmr.PhaseFailed {
		t.Errorf("phase is %q, want %q", phase, eqmr.PhaseFailed)
	}
	if results, err := ctrl.Results(ctx); err == nil {
		t.Fatalf("Results returned %v for a failed run", results)
	} else if !strings.Contains(err.Error(), reason) {
		t.Errorf("Results error %q does not preserve failure reason %q", err, reason)
	}
	kept, err := eq.Tasks(ctx, ctrl.QuarantineQ())
	if err != nil || len(kept) != 1 {
		t.Errorf("quarantine queue: got %d tasks, %v; want the failed task kept", len(kept), err)
	}
}

// TestQuarantinedLoserIsRetired pins the other half of the error worker's
// judgment: a quarantined duplicate whose split was already accepted is a
// loser, not a failure, and the run completes.
func TestQuarantinedLoserIsRetired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(2, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, histogram := wordCountFixture(t, 10, 100, 4)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}
	tasks, err := eq.Tasks(ctx, ctrl.MapQ())
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list map tasks: %d, %v", len(tasks), err)
	}
	dup := tasks[0]

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() { _ = ctrl.RunController(runCtx) }()
	go func() { _ = ctrl.RunMapper(runCtx, eqmr.WordCountMapper) }()
	go func() { _ = ctrl.RunReducer(runCtx, eqmr.SumReducer) }()
	if err := ctrl.Wait(runCtx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// A duplicate of a map task whose split was already accepted, quarantined
	// after exhausting its claims. It lost; it did not fail.
	errQ := ctrl.MapQ() + "/err"
	if _, err := eq.Modify(ctx, entroq.InsertingInto(errQ,
		entroq.WithRawValue(dup.Value),
	)); err != nil {
		t.Fatalf("seed quarantined duplicate: %v", err)
	}
	for {
		left, err := eq.Tasks(ctx, errQ)
		if err != nil {
			t.Fatalf("list %s: %v", errQ, err)
		}
		if len(left) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("quarantined duplicate was never judged")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if phase, reason, err := ctrl.Status(ctx); err != nil || phase != eqmr.PhaseDone {
		t.Fatalf("status after judging a loser: %q (%s), %v; want done", phase, reason, err)
	}
	if kept, err := eq.Tasks(ctx, ctrl.QuarantineQ()); err != nil || len(kept) != 0 {
		t.Errorf("quarantine queue: got %d tasks, %v; want the loser retired", len(kept), err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if got, want := kvStrings(results), expectedFromHistogram(histogram); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("results %v, want %v", got, want)
	}
}

// TestMaxClaimsDefault pins that a run gets a claim ceiling without asking, and
// that unlimited remains reachable. A default the documentation tells you to
// always override is not a default.
func TestMaxClaimsDefault(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), eqmr.WithMapShards(2), eqmr.WithReduceShards(2))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if got := ctrl.Config().MaxClaims; got != eqmr.DefaultMaxClaims {
		t.Errorf("default MaxClaims is %d, want %d", got, eqmr.DefaultMaxClaims)
	}

	unlimited, err := eqmr.New(eq, testPrefix(), eqmr.WithMaxClaims(-1))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if got := unlimited.Config().MaxClaims; got != -1 {
		t.Errorf("negative MaxClaims became %d, want it preserved as unlimited", got)
	}

	// A whole Config still works, for callers who prefer one, and composes with
	// other options rather than replacing them.
	viaStruct, err := eqmr.New(eq, testPrefix(),
		eqmr.WithLease(time.Minute),
		eqmr.WithConfig(eqmr.Config{MapShards: 4, ReduceShards: 2}))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if got := viaStruct.Config(); got.MapShards != 4 || got.ReduceShards != 2 || got.Lease != time.Minute {
		t.Errorf("WithConfig did not compose: %+v", got)
	}
}

// TestSetupIsRetryable pins that a run appears all at once and that Setup can
// be retried: a repeat with the same input, as after a lost reply, succeeds
// without duplicating any work, while reusing the Prefix for a different run
// is refused.
func TestSetupIsRetryable(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	prefix := testPrefix()
	ctrl, err := eqmr.New(eq, prefix, testOpts(3, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, histogram := wordCountFixture(t, 20, 200, 6)
	for i := range 2 {
		if err := ctrl.Setup(ctx, input); err != nil {
			t.Fatalf("setup %d: %v", i, err)
		}
	}
	tasks, err := eq.Tasks(ctx, ctrl.MapQ())
	if err != nil {
		t.Fatalf("list map tasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %d map tasks after a repeated Setup, want 3", len(tasks))
	}

	other, err := eqmr.New(eq, prefix, testOpts(2, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := other.Setup(ctx, input); err == nil || !strings.Contains(err.Error(), "different run") {
		t.Fatalf("setup of a different run on the same prefix: got %v, want a refusal", err)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = ctrl.RunController(runCtx) }()
	go func() { _ = ctrl.RunMapper(runCtx, eqmr.WordCountMapper) }()
	go func() { _ = ctrl.RunReducer(runCtx, eqmr.SumReducer) }()
	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if got, want := strings.Join(kvStrings(results), ","), strings.Join(expectedFromHistogram(histogram), ","); got != want {
		t.Fatalf("results %v, want %v", got, want)
	}
}

// TestDocKeysAreUnique pins that no two docs in a run share both a primary and
// a secondary key, which backends may enforce. Map-output pointers are the
// case that matters: one partition collects many runs from many splits under
// one primary key, so its pointers must differ in their secondary keys.
func TestDocKeysAreUnique(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(3, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, _ := wordCountFixture(t, 50, 3000, 6)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Small runs, so each split writes several runs per partition.
	mapOnly(ctx, t, eq, ctrl, eqmr.WithIntermediateRunBytes(512))
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = ctrl.RunController(runCtx) }()
	waitForPhase(ctx, t, ctrl, eqmr.PhaseReduce)

	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: ctrl.DocNS(), OmitValues: true})
	if err != nil {
		t.Fatalf("list docs: %v", err)
	}
	seen := make(map[[2]string]bool)
	perKey := make(map[string]int)
	for _, d := range docs {
		k := [2]string{d.Key, d.SecondaryKey}
		if seen[k] {
			t.Errorf("two docs share key %q and secondary key %q", d.Key, d.SecondaryKey)
		}
		seen[k] = true
		perKey[d.Key]++
	}
	shared := 0
	for key, n := range perKey {
		if strings.HasPrefix(key, "mapout/") && n > 1 {
			shared++
		}
	}
	if shared == 0 {
		t.Error("no partition collected more than one pointer, so the test proves nothing")
	}
}

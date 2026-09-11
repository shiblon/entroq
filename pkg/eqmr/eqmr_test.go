package eqmr_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/worker"
)

// testOpts is the standard option set for these tests: intervals tightened so
// the suite is paced by work rather than by the production-shaped control loop.
func testOpts(mapShards, reduceShards int) []eqmr.Option {
	return []eqmr.Option{
		eqmr.WithMapShards(mapShards),
		eqmr.WithReduceShards(reduceShards),
		eqmr.WithLease(30 * time.Second),
		eqmr.WithControlInterval(25 * time.Millisecond),
		eqmr.WithStallTimeout(60 * time.Second),
	}
}

// testPrefix returns a fresh, unique run prefix.
func testPrefix() string { return "/eqmrtest/" + entroq.GenHex16() }

func newClient(ctx context.Context, t *testing.T) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open in-memory client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
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

func TestCleanupRemovesEverything(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	prefix := testPrefix()
	ctrl, err := eqmr.New(eq, prefix, testOpts(3, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	input, _ := wordCountFixture(t, 50, 500, 6)
	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: ctrl.DocNS(), OmitValues: true})
	if err != nil {
		t.Fatalf("list docs: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("expected result docs before cleanup")
	}

	if err := ctrl.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	docs, err = eq.Docs(ctx, &entroq.DocQuery{Namespace: ctrl.DocNS(), OmitValues: true})
	if err != nil {
		t.Fatalf("list docs after cleanup: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("expected no docs after cleanup, got %d", len(docs))
	}

	stats, err := eq.QueueStats(ctx, entroq.MatchPrefix(prefix))
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	for name, stat := range stats {
		if stat.Size != 0 {
			t.Errorf("queue %q still holds %d tasks after cleanup", name, stat.Size)
		}
	}
}

func TestEmptyInputCompletes(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(4, 3)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := ctrl.Run(ctx, nil, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run with empty input: %v", err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for empty input, got %d", len(results))
	}
	phase, _, err := ctrl.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if phase != eqmr.PhaseDone {
		t.Errorf("phase is %q, want %q", phase, eqmr.PhaseDone)
	}
}

// TestQuarantinedWorkFailsRun pins the failure story the Mapper contract
// describes: once work has been quarantined, the controller fails the whole run
// with a reason rather than waiting on it forever.
//
// It seeds the quarantine queue directly rather than driving a poisonous record
// through repeated worker deaths. Reclaiming a dead worker's task is the
// backend's business and happens on its own schedule; what this package is
// responsible for is noticing the result and ending the run, and that is what
// gets tested here.
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

	// A map task that exhausted its claims would land here.
	if _, err := eq.Modify(ctx, entroq.InsertingInto(ctrl.ErrQ(),
		entroq.WithValue(map[string]string{"reason": "maximum claims exceeded"}),
	)); err != nil {
		t.Fatalf("seed quarantine queue: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() {
		_ = ctrl.ControlWorker().Run(runCtx,
			worker.Watching(ctrl.ControlQ()),
			worker.WithLease(5*time.Second),
		)
	}()

	err = ctrl.Wait(runCtx)
	if err == nil {
		t.Fatal("expected the run to fail with work in quarantine")
	}
	if !strings.Contains(err.Error(), "quarantined") {
		t.Errorf("error %q does not mention quarantine", err)
	}

	phase, reason, serr := ctrl.Status(ctx)
	if serr != nil {
		t.Fatalf("status: %v", serr)
	}
	if phase != eqmr.PhaseFailed {
		t.Errorf("phase is %q, want %q", phase, eqmr.PhaseFailed)
	}
	if !strings.Contains(reason, "quarantined") {
		t.Errorf("reason %q does not explain the failure", reason)
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

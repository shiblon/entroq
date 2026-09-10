package eqmr_test

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
)

// Example_singleProcess runs a whole MapReduce in one process. This is the
// shape for tests, small jobs, and getting a feel for the API; the deployed
// shape is the two examples below it.
func Example_singleProcess() {
	ctx := context.Background()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	// Shard counts are required, and only here: they fix the document layout
	// when the run is created and cannot change afterwards.
	ctrl, err := eqmr.New(eq, "/wordcount/run-1",
		eqmr.WithMapShards(2),
		eqmr.WithReduceShards(2),
	)
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	input := []*eqmr.KV{
		eqmr.NewKV("", "the quick brown fox"),
		eqmr.NewKV("", "the lazy dog"),
		eqmr.NewKV("", "the quick dog"),
	}

	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers:  2,
		Reducers: 2,
		Combiner: eqmr.SumCombiner, // optional: shrinks intermediate data
	}); err != nil {
		log.Fatalf("run: %v", err)
	}

	results, err := ctrl.Results(ctx)
	if err != nil {
		log.Fatalf("results: %v", err)
	}
	for _, kv := range results {
		fmt.Printf("%s=%s\n", kv.Key, kv.Value)
	}

	// Output:
	// brown=1
	// dog=2
	// fox=1
	// lazy=1
	// quick=2
	// the=3
}

// ExampleController_RunMapper shows a mapper pod.
//
// It needs the run prefix and nothing else. The partition count it must agree
// on arrives with each task, so this deployment cannot be configured out of
// step with the run it is serving.
func ExampleController_RunMapper() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	prefix := "/wordcount/run-1" // from the environment, in a real pod
	ctrl, err := eqmr.New(eq, prefix)
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	// Count the words in each record, emitting one pair per distinct word.
	countWords := func(ctx context.Context, _, value string, emit eqmr.EmitFunc) error {
		counts := make(map[string]int)
		for _, w := range strings.Fields(value) {
			counts[w]++
		}
		for w, n := range counts {
			if err := emit(ctx, w, strconv.Itoa(n)); err != nil {
				return err
			}
		}
		return nil
	}

	// RunMapper returns when the worker exits, which for a mapper is expected:
	// an error from the Mapper kills it, because a run whose map calls failed
	// cannot support a claim about its output. Restart it. The task is
	// reclaimed when its lease expires, and the claim ceiling turns a record
	// that kills every worker into a failed run rather than an endless one.
	//
	// In a pod, returning here and letting the orchestrator restart is fine too.
	cancel()
	if err := ctrl.RunMapper(ctx, countWords); err != nil {
		log.Printf("mapper exited: %v", err)
	}

	fmt.Println("mapper stopped")
	// Output: mapper stopped
}

// ExampleController_RunController shows a controller pod.
//
// Run as many replicas as you like: the control queue holds exactly one task,
// so a claim makes exactly one of them act at a time, and a replica that dies
// is replaced once its claim is released.
func ExampleController_RunController() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer eq.Close()

	ctrl, err := eqmr.New(eq, "/wordcount/run-1")
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	cancel()
	if err := ctrl.RunController(ctx); err != nil {
		log.Printf("controller exited: %v", err)
	}

	// Progress is readable from any process holding the prefix, so a status
	// endpoint needs nothing else. It reports counts rather than a per-worker
	// breakdown: units are claimed by whichever worker is free, so no worker
	// owns a knowable share. Per-worker throughput is a metrics question,
	// answered by entroq.worker.tasks_total.
	fmt.Println("controller stopped")
	// Output: controller stopped
}

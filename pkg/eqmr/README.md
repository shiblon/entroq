# eqmr: experimental MapReduce on EntroQ

`eqmr` is **experimental**. Its Go API and its queue and document layouts may
change without a migration path; do not treat what it writes as a stable
interchange format.

A run is coordinated entirely through EntroQ: work is tasks, run state is a
doc. Data may live in EntroQ too, for small jobs, but nothing assumes it does:
intermediate runs go through a store, and output goes to a `Sink`.

## Shape

```
        {prefix}/map              {prefix}/reduce
             |                          |
     +---------------+          +---------------+
     |  RunMapper    |          |  RunReducer   |
     |  (scale N)    |          | (scale to R)  |
     +---------------+          +---------------+
             |                          |
   {prefix}/map-done          {prefix}/reduce-done
             \                         /
              +-----------------------+
              |     RunController     |   also: {prefix}/map-check,
              |     (one per run)     |   /reduce-check, /gc, and
              +-----------------------+   every {queue}/err
                         |
   docs:  state, split/NNNNNN, mapout/NNNNNN, result/NNNNNN
          (document store and sink: run/..., output/...)
```

Mappers and reducers consume a task and commit a finish task in the `Modify`
that deletes it; they never write a doc. The controller is a set of workers
that react to finish tasks and alone write run state: work goes out as tasks,
comes back as finish tasks, and the controller decides what each one means.
Mappers and reducers scale on their own. Run one controller per run; one that
dies is recovered by restarting it, since its tasks wait in their queues and
its claims expire.

`Controller.Run` does all of this in one process, for tests, benchmarks, and
small jobs.

## A run

**Setup** stages the input as split (sharded) docs. It then creates the run, its
state doc and every map task, in one `Modify`. Setup is atomic and idempotent,
so it's never half done and can always be safely retried.

**Acceptance.** The controller accepts the first finish for each unit while
claiming only that unit's input, the split doc or the partition's pointer docs,
and deletes the input in the `Modify` that records the acceptance. Acceptances
of different units never wait on one another; a later finish for the same unit
finds its input gone.

**Checks.** Each acceptance commits a check task in the same `Modify`, and the
check decides whether the phase is over. Checks never touch the docs a phase
consumes, so the check committed by the last acceptance always finds them gone.
The first check to find them gone claims the state doc and moves the run on;
any check after it sees the phase has moved and retires without a claim.

| phase | ends when | ended by |
|---|---|---|
| `map` | no split docs remain | a map check, which fans out reduce tasks |
| `reduce` | no pointer docs remain | a reduce check, after finishing the sink |
| `done` / `failed` | terminal | |

Reduce tasks exist only for partitions that received data, and a run whose
input maps to nothing goes straight to `done`. Empty input is a `Setup` error.
The state doc holds only the phase and what `Setup` fixed; progress within a
phase is derived from the docs it consumes.

## Configuration

Both shard counts are required. `Setup` fixes the layout. Worker pods construct
their controller from the prefix, and mappers read the partition count from
their task:

```go
ctrl, err := eqmr.New(eq, prefix)                       // mapper or reducer pod
ctrl, err := eqmr.New(eq, prefix,                        // the launcher
    eqmr.WithMapShards(64),
    eqmr.WithReduceShards(8))
```

A store or sink is chosen with `WithIntermediateStore` or `WithSink` and
recorded in the run by its `Descriptor`. Every process that maps, reduces, or
controls the run must be configured with one that reports the same descriptor.
A long name in a descriptor, such as a long run prefix, is shortened to its
head, a short hash of the whole, and its tail.

## Shuffle

Mappers assign each key to a partition by `ShardForKey`, buffer up to
`DefaultIntermediateRunBytes` (`WithIntermediateRunBytes`), sort records by
primary key, secondary key, and value, and write immutable runs. The
controller publishes one pointer doc per run, keyed `mapout/<partition>` with
the split and run as its secondary key (`split/000003/0001`), so a split's
absence means its output is durable and reachable. Reducers heap-merge a
partition's runs and feed one key at a time through `ReducerInput`, so reduce
work scales with partitions.

Records also carry a secondary key, which is a sort dimension. The Mapper API
leaves it empty for now.

## Storage

- The run's document store, the default, keeps runs as chunk docs in the run's
  namespace, for small jobs.
- `NewFileStore(name, root)` keeps runs as files under `root`, one directory per
  run, each written once under a unique name and synced before its reference is
  returned, so nothing refers to an unfinished file. Every process must see the
  same files at the same path.
- Stores on the [object-store HTTP protocol](../../docs/storage-http.md) are
  under development; drivers store opaque streams.

Runs are written in checksummed blocks, chained and closed by a counted footer,
so damage is caught before any record of a damaged block is used, and
truncation is caught at the end. Every write names its run, and when the run
ends the controller tells the store to sweep it, however the store knows how.

## Output

A `Sink` owns the output's format, naming, checksums, and readiness. The run
hands it a value per key. Writes are staged and committed separately, so
duplicate and failed attempts are never visible:

1. A reducer streams its results into a staged attempt. Its finish task carries
   the sink's reference to the attempt.
2. The controller commits the first accepted attempt per partition. Any later
   commit, or a repeat after a crash, leaves the committed output alone.
3. When every partition with data is committed, the controller calls `Finish`,
   naming the partitions that received none, before marking the run done. A
   consumer that sees the sink's readiness marker needs nothing from EntroQ.
4. When the run ends, done or failed, the sink sweeps every uncommitted attempt.

Only the sink deletes staged attempts. A controller that commits and dies
before recording it can leave a finish that looks like a loser whose output is
in fact committed.

The default document sink suits small jobs, and `Results` and
`ResultsForPartition` read it. Other sinks are read with their own readers.

## Duplicates and backup tasks

Workers read their input without claiming it, so two may process one unit at
once; the controller accepts the first finish and collects the other's runs.
That makes backup tasks possible: insert a second task for a straggling unit
and the two race. `eqmr` ships no straggler policy; when to duplicate is a
scheduling question.

A duplicate that starts after its unit was accepted finds no input and retires.
A reducer whose inputs are collected mid-read cannot tell that from a store
failure, so it retries, and then finds no pointers and retires. A duplicate
that exhausts its claims anyway is recognized as a loser and retired.
`Progress` caps running units at the number outstanding,
so a duplicated unit counts once.

## Failure

A `Mapper` or `Reducer` that returns an error kills its worker, the MapReduce
contract: a run in which some calls failed cannot vouch for its output. The task
is reclaimed when its lease expires. `MaxClaims` (default `DefaultMaxClaims`,
10; negative for unlimited) bounds how often: past it the task is quarantined
to its queue's error queue, `{prefix}/map/err` for example. (`pkg/worker`
defaults to unlimited, since a claim count cannot tell crashes from ordinary
churn. A batch run has an end and can afford to fail.)

The controller's error worker judges every quarantined task: a duplicate whose
unit is already done is retired, and anything else fails the run with the
task's own error and moves to `{prefix}/quarantine` for inspection.

These fail the run at once, with a reason: text that is not valid NUL-free
UTF-8, a corrupt intermediate run, a sink error marked with a
`worker.MoveError`, and a store or sink a process cannot reach. The last is
checked when a controller starts and when a worker takes a task; the task is
consumed, and the submitter fixes the deployment and submits again. A backend
that is temporarily unavailable is retried.

Use `RunMapper`, `RunReducer`, and `RunController`: they watch the right queues
with the run's lease and claim ceiling. Of the controller workers, the
collector alone has a claim ceiling, so a store that stays unavailable cannot
hold `Run` open.

## Collection

The controller's `gc` worker deletes intermediate data. It deletes at once only
what a decision retired inside one EntroQ commit: a losing map finish's runs, a
reduced partition's consumed runs. Anything whose fate was settled outside
EntroQ, such as staged output, waits for the sweep that every run-ending commit
queues, of both store and sink. The sweep also collects what crashed workers
wrote and never reported. A failed sweep is logged; what it misses is garbage.
Collection never fails a run, and a collection task that exhausts its retries
is kept in quarantine.

## Progress

`Controller.Progress` reports per-phase counts (total, done, running, pending)
and the quarantine depth. Counts are per phase: units go to whichever worker
is free, so no worker has a share to report. Per-worker throughput is a
metrics question (`entroq.worker.tasks_total`,
`entroq.worker.slots`). A mapper commits once per split, so finer progress
comes from smaller splits.

## Combiners

A `Combiner`'s output has the same type as its input, so it may run several
times within one split. It must satisfy `C(C(a) ++ C(b)) == C(a ++ b)`: sum,
min, max, count, and set union qualify; mean and median only as a richer
intermediate. It is an optimization: `TestCombinerDoesNotChangeResults` pins
that output is unchanged.

## Keys and values are text

Keys and values are strings and must be valid UTF-8 with no NUL, checked by
`ValidateText` where they are produced: on input at `Setup`, on every emitted
pair and combined value, and by the document sink on every result. Document
content is JSONB in PostgreSQL, which rejects NUL, and a JSON string cannot
represent invalid UTF-8; Go's `encoding/json` silently substitutes U+FFFD. A
job with binary keys encodes them itself: measured,
base64 everywhere made map output 22% larger and unreadable.

## Cleanup

There is no `Cleanup` yet. A finished run keeps its state doc, its result
markers, its document-sink output if any, and its quarantined tasks if it
failed; intermediates are collected as it goes. Once the sink has finished,
nothing in EntroQ is needed to read the output.

A namespace under EntroQ's `/gc=` convention expires its unclaimed doc groups
at a time fixed when they are inserted: right for output with a bounded reading
window, wrong for intermediates.

## Known limits

- A split doc holds its input inline and must fit the backend message limit
  (10MB by default for the gRPC service).
- Mapper memory is bounded by `IntermediateRunBytes`, except for a single
  record. A reducer holds one block or chunk per input run; many small runs
  raise that, which a later map-phase merger can reduce.
- With the document store, a single record must fit the backend message limit,
  and the document sink buffers a whole partition before staging it.
- A map finish names every run its split produced, and `Setup` creates every
  map task in one commit, so extreme map expansion and very many splits are
  bounded by the backend message limit. Range tasks, one per batch of splits,
  will lift the latter.
- A Combiner works within one mapper run; values for a key from different runs
  or splits reach the reducer separately.
- `Setup` refuses a prefix that already holds a different run: use a fresh
  prefix per run.

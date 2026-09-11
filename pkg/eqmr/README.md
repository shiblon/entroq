# eqmr: experimental MapReduce on EntroQ

`eqmr` is an **experimental** package. Its Go API, doc layout, and queue layout
may change or be removed without a migration path. Do not treat what it writes
to a namespace as a stable interchange format.

It runs a complete MapReduce with no storage outside EntroQ: input, shuffle
map outputs, and output all live as docs, and every phase transition is a task.

## Deployed shape

Nothing in a run needs to know about anything else except the queue names, so
each role scales independently:

```
                 {prefix}/map          {prefix}/reduce        {prefix}/control
                      |                      |                       |
              +---------------+      +---------------+       +---------------+
              |  RunMapper    |      |  RunReducer   |       | RunController |
              |  (scale N)    |      | (scale to R)  |       | (scale 1..k)  |
              +---------------+      +---------------+       +---------------+
                      |                      |                       |
   docs:  split/NNNNNN  ->  mapout/NNNNNN  ->  result/NNNNNN
```

One process does `Setup` to write the splits and create the control task. After
that, mapper and reducer deployments can be scaled on queue depth, and any
number of control pods may run: the control queue holds exactly one task, so
EntroQ's claim guarantees exactly one controller is acting at any moment, and a
controller that dies is replaced when its lease expires.

`Controller.Run` does all of this in a single process. That is for tests,
benchmarks, and small jobs, not for the deployed shape.

## Phases

| phase | barrier | what ends it |
|---|---|---|
| `map` | no `split/` docs remain | every mapper committed its map outputs |
| `reduce` | every partition has a result doc | every reducer committed |
| `done` / `failed` | terminal | control task parks, stays inspectable |

Completion is read **purely from documents**. A split document disappears when
its mapper commits; a result document appears when a partition commits, empty
partitions included, which is why an empty partition still writes an empty result
document rather than silently vanishing. No queue is consulted to decide whether
a phase is finished.

## Progress

`Controller.Progress` reports per-phase counts (total, done, running, pending)
plus the quarantine depth, enough to drive a status view.

It deliberately does not break down by worker. Classic MapReduce could draw a
progress bar per worker because input shards were statically preassigned, so
"how far along is this mapper" had an answer. EntroQ is competing-consumer: a
unit is claimed by whichever worker is free, so no worker owns a knowable share
and there is no per-worker denominator to fill. Per-worker throughput and
idleness are observability questions, answered by `entroq.worker.tasks_total`
and `entroq.worker.slots` rather than by run state.

Progress resolution is a function of `MapShards`. There is no sub-split
reporting, because a mapper commits once at the end of its split, so finer
granularity comes from cutting smaller splits rather than from progress
messages.

Both barriers are sound for the same reason: a worker deletes its input doc in
the *same* `Modify` that writes its output docs. So "no split docs remain"
atomically implies "every map-output doc exists". No coordinator state is required,
and a worker that dies mid-task simply loses its claim.

## Shuffle

Mappers assign each intermediate key to one of the run's reduce partitions
by `ShardForKey`, and write one map-output doc per non-empty partition, keyed
`mapout/<partition>`. A reducer claims a whole partition in one `ClaimDocs`,
merges the per-split sorted runs, and reduces each key once.

Map-output docs carry **no secondary key**, on purpose. A secondary key keeps subsets
of a primary-key group located together; it does not define the group. Here the
group is the whole partition, taken in one `ClaimDocs`, and it has no subset that
needs co-locating: the merge is order-independent and the reducer sorts values
itself. The field is left free rather than filled with something nothing reads,
so a later job-specific use (MapReduce's classic one being secondary sort) still
has it.

That makes reduce work proportional to the partition count, not to the number of
distinct keys.

Both shard counts are **required**, and required only by `Setup`, because that
is the moment the layout gets fixed. A worker pod therefore constructs its
controller from a prefix alone:

```go
ctrl, err := eqmr.New(eq, prefix)                       // mapper or reducer pod
ctrl, err := eqmr.New(eq, prefix,                        // the launcher
    eqmr.WithMapShards(64), eqmr.WithReduceShards(8))
```

Mappers do need the partition count, to decide where each emitted key goes, but
they read it from their own task rather than from configuration. Two deployments
configured with different numbers would each be internally consistent and jointly
wrong, silently; there is one writer instead, and no way to disagree.

## Combiners

A `Combiner` differs from a `Reducer` in the way that matters: its output has
the same type as its input, so it is closed over its own output and can run more
than once, at more than one stage. It must satisfy

```
C(C(a) ++ C(b))  ==  C(a ++ b)
```

Sum, min, max, count, and set-union qualify; mean and median do not unless
carried as a richer intermediate. A Combiner is purely an optimization:
`TestCombinerDoesNotChangeResults` pins that a run produces identical output
with and without one.

## Keys and values are text

Everything in the pipeline is a string: document keys (`split/000007`), map keys,
and values. All must be valid UTF-8 with no NUL.

Both halves of that rule are load-bearing. Keys and values live inside document
content, which is JSONB in PostgreSQL, and JSONB rejects `\u0000` outright
("unsupported Unicode escape sequence"). A JSON string cannot represent invalid
UTF-8 at all, and Go's `encoding/json` silently substitutes U+FFFD rather than
failing, so an unvalidated value would corrupt in transit rather than erroring
where the mistake was made.

A job whose keys or values are genuinely binary encodes them itself, with base64
or anything else. That choice belongs to the job, which knows whether it needs
it. The alternative, `[]byte` everywhere, charges every job for the few that
need it: measured on a real run, base64 made map-output documents **22% larger**
(18,550 bytes against 14,392) and left them unreadable:

```json
[{"key":"dzAwMDAwMA==","values":["MQ==","Mw=="]}]     // []byte
[{"key":"w000000","values":["1","3"]}]                // string
```

`ValidateText` is the rule, and it is enforced where violations happen: on input
at `Setup`, and on every emitted pair. An invalid pair is a `MoveError`, since
text that is not valid UTF-8 will never become valid on a retry, so the task is
quarantined at once and the controller fails the run with a reason rather than
killing a worker repeatedly over input that cannot succeed.

## Backup tasks (speculative execution)

Workers **read** their input document; they do not claim it. Exclusion happens
when they commit, through a version-pinned delete of that document. So two
workers may process the same unit at once, and the first to commit wins: the
loser's whole modification is rejected atomically, its output is never written,
and it costs duplicated compute and nothing else.

That makes the classic backup-task strategy available. Insert a second task for a
unit that has been in flight too long and the two race, rather than the duplicate
blocking on a claim the original still holds. `eqmr` deliberately ships **no
straggler policy**: deciding when a task is late enough to duplicate is a
scheduling question, and the mechanism is useful without an opinion about it.

A loser leaves its task claimed until the lease expires, after which it is
reclaimed, finds no input document, and retires. That is deliberately not a
retry: a retry would increment the attempt count, and a worker that merely lost
a race must never be quarantined, since quarantining anything fails the run.

An empty reduce partition has no input document to serve as the exclusion token,
so its result document carries an explicit id instead. A duplicate insert of the
same id is rejected, which gives the empty case the same guarantee.

Because `Running` can no longer be read from claimed documents, `Progress` counts
claimed **tasks** and caps them at the number of units actually outstanding, so a
duplicated unit shows as one running unit rather than two.

## Failure

A `Mapper` or `Reducer` that returns an error kills its worker. That is the
standard MapReduce contract: a run in which some map calls failed cannot support
any claim about the correctness of its output. The task is reclaimed once the
lease expires, so a transient failure costs one claim rather than the run.

A fatal handler error does not mark the task, so without a bound a genuinely
poisonous record is retried forever, killing a worker each time. `Config`
therefore defaults `MaxClaims` to `DefaultMaxClaims` (10): the task is
quarantined to `{prefix}/err` after ten claims and the controller fails the run
with a reason. Set it negative for unlimited.

`pkg/worker` leaves this unlimited, and that difference is deliberate.
`Task.Claims` conflates work that kills workers with ordinary infrastructure
churn such as rolling deploys and evictions, and that conflation is forced
rather than sloppy: a crashing worker is precisely the one that cannot reliably
record why it died. A general worker cannot know a workload's ratio of the two.
A batch run can take the position that a general worker cannot, because it has a
defined end and failing loudly beats burning a pool forever.

Use `RunMapper`, `RunReducer` and `RunController` rather than wiring workers by
hand: they watch the right queue with the run's lease and claim ceiling, so
nothing about the wiring is yours to get right. `RunController` deliberately
carries no claim ceiling, because the control task is claimed once per tick for
the life of the run and any bound would quarantine the controller after seconds
of healthy operation.

When a dead worker's task actually gets reclaimed is the backend's business, not
this package's. Let the claim mechanism do its job and roll with a blocked
claim.

The controller also fails a run whose phase makes no progress for
`Config.StallTimeout`, so a wedged run reports something rather than hanging.

## Cleanup

`Controller.Cleanup` removes every doc and task a run created. It refuses to
tear down a live run, because a claimed doc or task cannot be deleted.

A run whose output should simply expire can instead put its namespace under
EntroQ's `/gc=` convention. Note the activation timestamp is baked into the
namespace string at insert time and collects any unclaimed doc group once it
fires, which is right for output a consumer has a bounded window to read and
wrong for intermediates.

## Known limits

- A split doc holds all of its input KVs in one value, and a map-output doc holds one
  partition's output from one split. Both must fit the backend message size
  limit (10MB by default for the gRPC service).
- A reducer holds an entire partition in memory. **`ReduceShards` is the knob
  for this**: more partitions means smaller ones. A merger worker would not
  help, because `ClaimDocs` returns document content and `DocClaim` has no
  `OmitValues`, so claiming a partition materializes all of it either way.
- The `Combiner` runs only within a split. Values for one key emitted by
  different splits accumulate untouched until the reducer. A merger worker that
  combined map-output documents across splits, concurrently with the map phase, would
  close that gap and cut intermediate volume; it is worth doing for throughput
  and map output size, but not as a memory bound. Inserting a new map output into a
  partition whose documents are claimed does work (doc inserts use generated
  IDs, and only an explicit-ID collision is rejected), so such a merger can run
  alongside mappers safely.
- `Setup` is not idempotent; use a fresh `Prefix` per run.

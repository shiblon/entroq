# EntroQ JavaScript/TypeScript Client

Workers for the [EntroQ](https://github.com/shiblon/entroq) task queue, written
in JavaScript or TypeScript and run through the work gateway, `eqlink work`.

The gateway runs the EntroQ worker loop in Go: it claims tasks, renews them,
claims doc sets, fixes up versions at commit, and retries, moves, and backs off.
A worker here answers its small JSON protocol over HTTP and holds no EntroQ
state of its own, so it has nothing to get wrong about leases or claims. The
protocol is [`docs/workgateway-protocol.md`](../../docs/workgateway-protocol.md).

## Installation

```bash
npm install entroq
```

It has no runtime dependencies. It needs Node.js 20 or later, and `eqlink` on
`PATH` (or named with `eqlink`) for `entroq/node` to start.

## Quick start

```typescript
import { Worker, M } from "entroq";
import { NodeGateway } from "entroq/node";

const worker = new Worker(["/my/queue"], {
  work: async (w) => {
    console.log("Processing:", w.task.value);
    // Commit atomically: here, delete the task to finish it.
    return M.of(M.deleting(w.task));
  },
});

await new NodeGateway({ entroq: "localhost:37706" }).run(worker);
```

`NodeGateway` starts one `eqlink work` child for the process, shared by every
worker, and stops it when they are done. The first SIGINT or SIGTERM drains:
every task in hand finishes and nothing more is claimed. A second hangs up at
once, leaving tasks in hand to their leases.

## Workers

A `Worker` is one session with the gateway, holding at most one task at a time.
For more at once, or for several kinds of work, run several:

```typescript
await gateway.run(
  new Worker(["/a"], { work: workA }),
  ...Array.from({ length: 4 }, () => new Worker(["/b"], { work: workB })),
);
```

Each session claims as a consumer of its own, so workers never hold each
other's doc sets.

### The functions

Only `work` is required.

- `work(w, ctx)` settles one task. What it returns is committed: a
  `Modification`, or nothing to commit nothing. The task in hand is not
  deleted unless the modification says so.
- `take(w, ctx)` names the doc sets the task needs, with `claimSet(ns, key)`.
  They are claimed together, all or none, before `work` runs, and `w.docs`
  holds their members. A set someone else holds sends the task back with
  backoff.
- `onSuccess(w, ctx)` runs after a commit lands. It is at most once, so what it
  does must be safe to skip.
- `onDependency(w, deps, ctx)` runs when a commit loses a dependency race.

Throw instead of returning to give the task another fate:

- `RetryError(message, orMoveTo?)` tries it again later, counting the attempt;
  once the attempts run out it moves to `orMoveTo` or the error queue.
- `MoveError(message, queue?)` moves it now, to `queue` or the error queue.
- `FatalWorker(message)` stops every worker; the task comes back when its lease
  lapses.
- Anything else stops this worker, after the gateway quarantines the task if
  this was its last allowed claim.

`w` carries `task`, `docs`, and `sets`. `ctx` carries
`signal`, aborted once the worker starts stopping; `stop()`, to stop once this
task is settled; `sleep(ms)`, which wakes early when stopping; and `read(kind,
fields)` and `time()`, reads the gateway answers mid-turn.

Long work needs nothing extra: the client tells the gateway it is still working
while a function runs.

### Modifications

`M.of(...)` commits its operations together, or not at all:

```typescript
M.of(
  M.deleting(w.task),
  M.inserting({ queue: "/next", value: { n: 1 } }),
  M.inserting({ queue: "/later", value: 2, byMs: 60_000 }),
  M.changing(w.docs[0], { content: { count: 3 } }),
  M.depending(otherDoc),
);
```

Name a task or doc as it was handed over; the gateway moves the versions of the
task in hand and its claimed docs to wherever renewal left them.

### Options

```typescript
new Worker(queues, handlers, {
  workTimeoutS: 300, // how long one task should take; the operator caps it
  maxAttempts: 0,    // retries before quarantine; 0 is no limit
  maxClaims: 0,      // claims a task may fail to survive; 0 is 5, negative is none
  retryDelayS: 0,    // base retry delay, and the wait after doc contention; 0 is 30s
  errorQueue: "",    // where moved tasks go; "{inbox}/err" by default
});
```

Durations are whole seconds.

## A gateway you did not start

`entroq` alone uses nothing but `fetch`, `AbortSignal`, and timers, so it runs
anywhere those do. Given the URL of an `eqlink work --addr` already serving:

```typescript
import { Gateway, Worker } from "entroq";

const gateway = new Gateway({ url: "http://127.0.0.1:43117/" });
await gateway.run(worker);
gateway.stop(); // drains, from elsewhere
```

`npm run typecheck` checks that it type-checks with browser types alone. A
browser worker would also need the gateway to serve CORS and to sit behind
authentication, since whoever reaches it can work its queues; neither is built
yet.

## Development

```bash
npm install
npm run typecheck
npm test   # the integration tests build eqlink and run eqmem with `go run`
npm run build
```

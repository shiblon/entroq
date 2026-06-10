# EntroQ JavaScript/TypeScript Client

A client for the [EntroQ](https://github.com/shiblon/entroq) task queue, for
Node.js and other modern JavaScript runtimes. It talks to an EntroQ service over
the HTTP/JSON API and provides both a low-level client and a worker abstraction
that renews task leases for you.

## Installation

```bash
npm install entroq
```

The package is published as `entroq`. It has no runtime dependencies.

## Quick start

```typescript
import { EntroQClient, EntroQWorker } from "entroq";

const client = new EntroQClient({ baseUrl: "http://localhost:9100" });
const worker = new EntroQWorker(client);

await worker.run(["/my/queue"], async (task) => {
  console.log("Processing:", task.value);
  // Return a modification to apply atomically; here we delete to finish.
  return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
});
```

## EntroQClient

The low-level client. It handles the wire protocol, generates a claimant ID, and
exposes the task and document operations.

```typescript
const client = new EntroQClient({
  baseUrl: "http://localhost:9100", // required
  claimantId: "my-worker",          // optional; a random id is generated otherwise
  headers: { Authorization: "Bearer …" }, // optional; sent on every request
});
```

Tasks:

- `time()` — server time, ms since epoch (as a string).
- `queues(prefix?, exact?, limit?)` / `queueStats(...)` — queue listing and stats.
- `tasks(request)` — list tasks in a queue.
- `tryClaim(queues, durationMs?)` — claim a task if one is available now, else
  `undefined`.
- `claim(queues, durationMs?, pollMs?)` — block until a task is claimed.
- `modify(request)` — apply an atomic batch of `inserts`/`changes`/`deletes`/
  `depends` (and the `doc*` equivalents). Returns the inserted/changed tasks.
- `streamTasks(request)` — async iterator of tasks (NDJSON over chunked
  transfer).

Documents (shared state in the same transaction space as tasks):

- `docs(request)` — list docs in a namespace.
- `claimDocs(request)` — atomically claim all docs sharing a primary key.
- `namespaceStats(request?)` — per-namespace doc stats.

## EntroQWorker

Claims tasks from one or more queues and dispatches each to a handler, renewing
the claim (and any claimed docs) in the background while the handler runs.

```typescript
const worker = new EntroQWorker(client, {
  leaseMs: 30000,      // claim/renewal duration (default 30s)
  pollMs: 5000,        // poll interval when a queue is empty (default 5s)
  retryDelayMs: 30000, // default delay for EntroQRetryError (default 30s)
  errQueue: "",        // default destination for EntroQMoveError
  maxAttempts: 0,      // >0 moves over-attempted tasks to errQueue (default off)
});
```

A handler is an `async (task, docs) => …` that either returns a modify request
to apply atomically, or returns nothing (`void`) to finish via a finisher:

```typescript
// Plain function: do work, return the modification to apply.
await worker.run(["/my/queue"], async (task) => {
  await handle(task.value);
  return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
});
```

To claim docs before work, or to finalize when the handler returns `void`, build
a handler and chain `.selector()` / `.finisher()`:

```typescript
const handle = EntroQWorker.handler(async (task, docs) => {
  // docs are the claimed docs, sorted by (primary key, secondary key).
  return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
})
  .selector(async (task) => [{ namespace: "config", key: `${task.queue}/settings` }])
  .finisher(async (task, docs) => { /* runs when doWork returns void */ });

await worker.run(["/my/queue"], handle);
```

Control flow from inside a handler:

- `throw new EntroQStopWorker()` — stop the loop cleanly after this task.
- `throw new EntroQRetryError(msg, delayMs?)` — re-queue this task after a delay.
- `throw new EntroQMoveError(msg, queue?)` — move this task to an error queue.

Call `worker.stop()` from elsewhere to unblock a waiting claim and exit after the
current task finishes.

## Error handling

A failed modification raises an **`EntroQDependencyError`** (HTTP 409 Conflict):
the version you pinned drifted, the task you targeted was already claimed or
deleted, or an insert ID collided. Re-read the affected tasks and retry. The
error groups the failed IDs by action:

```typescript
import { EntroQDependencyError } from "entroq";

try {
  await client.modify({ deletes: [{ id, version, queue }] });
} catch (err) {
  if (err instanceof EntroQDependencyError) {
    console.log("conflicting deletes:", err.deletes); // also .inserts/.changes/.depends/.claims
  }
}
```

## Development

### Tests

Unit tests mock the network; integration tests run against a real, ephemeral
EntroQ server.

```bash
npm test                  # unit + integration
npx vitest run --coverage # with coverage
```

The integration tests require the **Go** toolchain: they start the in-memory
server with `go run ./cmd/eqmem` and are skipped automatically if `go` is not on
`PATH`.

## Architecture

- **`EntroQClient`** — low-level HTTP/JSON client; wire protocol, claimant IDs,
  RESTful transcoding via Vanguard.
- **`EntroQWorker`** — worker abstraction with background lease renewal.
- **Streaming** — real-time task listing via HTTP chunked transfer (NDJSON).

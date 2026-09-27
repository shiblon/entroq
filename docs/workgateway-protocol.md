# Work Gateway Protocol

The work gateway (`eqlink work`, package `workgateway`) lets a worker written in
any language run the EntroQ worker loop without importing EntroQ, gRPC, or the
queue API. The gateway runs the hard, stateful part in Go — claim, renew at half
the lease, stop-and-freeze before commit, version fix-up, retry/move/backoff,
doc-claim ordering — and a foreign worker connects and answers a small
newline-delimited JSON protocol.

There are two kinds of user, and the protocol is shaped for both:

- A **library author** wraps this protocol in a client library for their
  language, hiding pipes/WebSockets and reconnection behind a "register handlers,
  run" API. This document is written for them.
- A **library user** imports that library, registers handler functions, and runs.
  They should never see any of what follows — it should feel like writing HTTP
  handlers.

The exact wire types live in `pkg/workgateway/protocol.go`; the assertions in
`workgateway_test.go` double as executable examples. This note is the contract
and the rationale.

---

## The payloads are the EntroQ protobufs, as protojson

A worker hand-models nothing. Every domain object on the wire — task, docs,
modification, dependency list — is the canonical protojson of the corresponding
message in `api/entroq.proto` (`Task`, `Doc`, `ModifyRequest`, `ModifyDep`). A
foreign worker generates those types from the same proto the rest of EntroQ uses.
Only the thin envelope around them — a `"type"` discriminator and the phase
framing below — is gateway-specific.

A worker does **not** fill in a modification's `claimant_id`: the gateway owns the
claim, attributes the commit itself, and ignores whatever the worker put there.
That attribution also applies to a document inserted with a future `atMs`: the
gateway creates it atomically under its own claim, just like an in-process Go
worker using `WithDocArrivalTime`.

## Registration is out of band

The queues a worker serves, its max-attempts and max-claims, which phases it
implements, its error queue, and its base retry delay are fixed for the session
and supplied at connection time, never as a wire message: flags for a spawned
pipe gateway (`--queue`, `--max-attempts`, `--max-claims`, `--take-docs`,
`--work`, `--success`, `--dependency`, `--error-queue`, `--retry-delay`), URL
query params for a WebSocket (`queue`, `maxAttempts`, `maxClaims`, `takeDocs`,
`work`, `success`, `dependency`, `errorQueue`, `retryDelay`). A work handler is
required; the others are opt-in. In the error queue, `{inbox}` stands for the
task's own queue, so `{inbox}/err` is the default; a name without it is one
error queue for every inbox.

## Hello and protocol versions

The gateway speaks first, before any task arrives, with a hello:

```
gateway -> hello {protocol: 1, version: "v1.13.0"}
```

`protocol` is the version of this protocol the gateway speaks, and it alone
decides compatibility: a worker checks it and stops if it does not speak it,
naming `version` (the gateway's release, for a person to read) in its error. A
gateway older than protocol 1 sends no hello; its first message is a phase
message, which a worker reads as protocol 0. The hello is sent even when the
registration is then refused, so a worker can tell a registration it got wrong
from a gateway it cannot talk to.

Protocol 1 added the hello, doc sets in `doWork`, the `error` outcome, and
`abort`.

## Phases

One connection is one worker slot: exactly one task in flight, strict
request/response, no correlation IDs. Concurrency is more connections. Per task,
the gateway sends only the phases the worker registered:

```
gateway -> hello {protocol, version}  # once, first
  (gateway claims a task and begins renewing it)
gateway -> takeDocs {task}            # only if registered takeDocs
client  -> docs {claims: [...]}
gateway -> doWork {task, docs, sets}
gateway -> abort {id, version}        # only if the claim is lost mid-task
client  -> result {outcome, ack?, modification?}
  (gateway stops renewal, freezes the stable version, commits atomically)
  (then exactly ONE post-commit phase fires, and only if registered)
gateway -> success {}                 # the commit succeeded
client  -> done {outcome}
gateway -> dependency {deps: [...]}   # the commit lost a dependency race
client  -> done {outcome}
  (loop)
```

Pre-commit phases (`takeDocs`, `work`) are named for the action the worker
performs; post-commit phases (`success`, `dependency`) for the outcome that fired
them. The two post-commit phases are mutually exclusive: exactly one can fire, and
only if registered.

The commit is the exactly-once boundary. Everything before it is at-least-once (a
dropped connection reclaims the task on lease expiry); `success` after it is
best-effort and at-most-once, so success-phase side effects must be idempotent or
safe to skip.

The worker answers only what it is asked: exactly one reply to each request,
and nothing else.

| Gateway sends | Worker replies |
|---|---|
| `takeDocs` | `docs` |
| `doWork` | `result`, even after an `abort` |
| `success` | `done` |
| `dependency` | `done` |
| `hello`, `abort`, `error` | nothing |

The gateway reads the worker's side of the connection all the time, so it
notices a worker that hangs up even while it is claiming or committing, and it
treats a message sent when no request is outstanding as a protocol violation (a
`caller` fault). A second reply to one request, a reply to `abort`, and a
message sent unprompted are all violations. A worker that hangs up never
interrupts a commit already under way: a worker may send its `result` and exit,
and the result still commits.

### Abort

If the gateway loses its claim while the worker is working (the lease lapsed,
say, and another worker claimed the task), it sends `abort`, naming the task by
`id` and `version`. It is one-way. The worker should stop that task's work if it
can, and it still sends its `result`, which the gateway discards: the task is no
longer the gateway's to commit, and one reply per request is what keeps the
stream in step. A worker that does not answer within a lease of the abort is a
`caller` fault, and the session ends.

A worker that handles one task at a time without reading meanwhile sees the
`abort` only after it has answered, when it reads the next message. It ignores an
`abort` that names a task it is not working on.

### Doc sets in `doWork`

Each claim in `docs` takes a doc set: the docs sharing a primary key in a
namespace, which have one version, claimant, and arrival time between them.
`sets` carries each claimed set, in the order the gateway claimed them
(sorted by namespace, then key), as the protojson of the `Doc` standing for
the set (no `id`, `secondaryKey`, or `content`) with its docs beside it:

```json
{"namespace": "ns", "key": "k", "version": 3, "claimant": "...", "atMs": "...",
 "len": 1, "docs": [{"namespace": "ns", "id": "a", "key": "k", ...}]}
```

`len` counts the set's docs. A set claimed with no docs appears with
`"docs": []`, still reporting its version and claim. `docs` is every set's
docs in one list, as before protocol 1. As protojson, a zero is omitted, so
read a missing `version` or `len` as 0.

### Outcomes and the `ack` shorthand

A `result` (and a post-commit `done`) carries an outcome: `ok`, `retry`, `move`,
`fatal`, or `error` — the vocabulary a native Go worker has. `error` reports
that the handler failed in a way it does not understand, such as an exception
it did not expect, as opposed to a retry, move, or fatal it chose: the gateway
stops as the Go worker does for an unknown handler error, leaving the task for
its lease to release, and exits with the caller class; the worker, which holds
the actual exception, raises it. `ok` commits the
modification; `ok` **alone does not delete the task**. Deleting the input is the
overwhelmingly common case, so set `"ack": true` and the gateway deletes the
claimed task for you (from its own authoritative copy — you never echo id/version
back). `ack` composes with other modifications; if the modification already
disposes of the input task (a change, delete, or depend on its id), the
modification wins and the `ack` is suppressed.

---

## Errors, exits, and one shared taxonomy

A native Go worker inspects a returned error's type and branches. A foreign worker
can't, so the gateway surfaces the same information as data, keyed on one small
set of **classes**:

| Class | Meaning | Author action |
|---|---|---|
| `ok` | clean stop: graceful shutdown, or the client hung up | stop; do not restart |
| `transient` | backend blip (EntroQ down / restarting / relocating) | retry / reconnect |
| `caller` | caller fault: bad registration, protocol violation, worker-requested `fatal` | stop and surface; a human fixes it |
| `gateway` | unexpected gateway-internal error | stop and surface; likely a bug |

The class is surfaced three ways:

- **`error` message**, mid-session, in place of the gateway's next phase message:
  `{"type":"error","class":...,"message":...}`. It is one-way and reply-free — the
  client acts (keep reading, restart the gateway, shut down), it does not answer.
  It reports errors that did **not** themselves drop the connection: a transient
  outage being retried, or the cause of a caller/gateway stop just before it
  happens. (A dropped connection can't be reported — there's no one to tell.)
- **Process exit code** over a pipe, from `sysexits.h`: `0` clean, `75`
  EX_TEMPFAIL (transient), `78` EX_CONFIG (caller), `70` EX_SOFTWARE (gateway).
- **WebSocket close code**: `1000` normal, `1013` try-again (transient), `1008`
  policy violation (caller), `1011` internal error (gateway).

Branch on the **class**, never the exact code. A decodable message of the wrong
type or shape is a `caller` fault; a message that fails to decode at all is
treated as a lost connection (a clean stop) rather than parsed for blame.

## The gateway rides out a restarting backend

EntroQ's gRPC service being restarted or relocated by an orchestrator is routine
control-plane churn, not a fault. The gateway rides it out: within
`--entroq-timeout` (default 60s) it reconnects with backoff, transparently — the
client sees a *pause* (and an optional `transient` `error` message), never a
disconnect. Past the timeout it gives up and exits `transient`, handing the
longer-horizon retry to the client's supervisor. This preserves worker
*liveness*, not the in-flight *task*: if the outage outlasts the lease, that task
reclaims elsewhere on the backend's return (at-least-once covers it).

Point the gateway at a **stable** target (a DNS or Kubernetes Service name, not a
pinned IP) so gRPC re-resolves and reconnects underneath during a relocation.

---

## The failure contract

What the gateway does when something goes wrong, and what becomes of the task.
"Released" means available to another worker at once; "its lease" means held
until the lease lapses, then claimable again. Each row but the last has a test
in `pkg/workgateway` (`contract_test.go` unless noted).

| Situation | Gateway | Task | Class |
|---|---|---|---|
| Worker hangs up while the gateway waits for a task | stops claiming at once | none claimed | `ok` |
| Worker hangs up with `takeDocs` or `doWork` unanswered | releases the task | released | `ok` |
| Worker sends its `result`, then hangs up | commits the result | committed | `ok` |
| Worker hangs up during `success` | stops (`workgateway_test.go`) | committed | `ok` |
| Worker hangs up during `dependency` (the commit failed) | stops | its lease, as after any failed commit: the lease is the backoff | `ok` |
| Worker sends a message with no request outstanding | drains: a commit under way finishes, nothing more is claimed | committed, if a commit was under way | `caller` |
| Worker replies with the wrong message type | stops (`workgateway_test.go`) | its lease | `caller` |
| Worker sends a message that does not decode | treats it as a hang-up (`workgateway_test.go`) | released | `ok` |
| Worker reports the `error` outcome | stops (`protocol1_test.go`) | its lease | `caller` |
| The claim is lost while the worker works | sends `abort`, discards the `result` | the new holder's | `gateway`, for now[^w5] |
| No `result` within a lease of an `abort` | ends the session | the new holder's | `caller` |
| EntroQ is unreachable | reports `transient`, rides it out for `--entroq-timeout` (`reconnect_test.go`) | its lease | `transient` if it gives up |
| Operator's first `SIGTERM`/`SIGINT` | drains, closing WebSocket connections normally (also `ws_test.go`) | committed as usual | `ok` |
| Operator's second signal | stops at once (`cmd/eqlink/cmd/drain_test.go`) | its lease | `ok` |
| The gateway itself crashes | nothing: the worker sees the connection end | its lease | none |

[^w5]: Once the Go worker keeps running after a lost claim, the session will
continue instead.

## Handling disconnects: two recipes

The client holds no EntroQ state — the gateway owns the claim, the lease, and the
exactly-once commit — so on any disconnect the client has nothing to roll back.
The in-flight task reclaims itself when the lease lapses. Two recipes, chosen by
deployment:

### Crash-only (decomposed / orchestrated services)

When the gateway exits, let the worker process exit too, and let the platform you
already run under restart it (`systemd Restart=on-failure`, Kubernetes
`restartPolicy`, `docker --restart`). The author writes **zero** reconnect code.
Key the restart policy on the exit code: restart on `75`, stay down on `0`, `78`,
`70`. Safe because the lease redelivers any in-flight task.

### Resident with reconnect (monoliths / long-lived processes)

When the worker is embedded in a process that must **not** die on a backend blip
(crash-only there would be a self-inflicted DOS), reconnect in place. Because the
gateway computes the class for you, the loop is not a decision tree — it is a
fixed shape:

```
while (run_one_connection() == RETRY) { sleep(backoff()) }
```

`RETRY` is the `transient` class (exit 75 / close 1013); everything else stops.
For a pipe, respawn a fresh `eqlink work` child (a new process is a new slot); for
WebSocket, re-dial `/work`. A fresh connection needs no replay — the protocol is
stateless per connection, so it just claims the next task.

### Always

- **Effects must be idempotent.** Any disconnect can redeliver the in-flight
  task, so `doWork` and `success`-phase side effects must be idempotent, or made
  exactly-once through the commit (consume the input with `ack`, use deterministic
  output IDs).
- **Inherit the gateway's stderr.** stdout carries only the protocol; diagnostics
  go to stderr. A spawning client must inherit it (Go's `os/exec` sends a child's
  stderr to `/dev/null` unless you set `cmd.Stderr`; Python/shell inherit by
  default) or lose all diagnostics.
- **To stop cleanly**, finish the exchange in progress (send the `result`, or
  the `done` of a post-commit phase), then hang up: close the gateway's stdin
  or the WebSocket, and wait for the gateway to exit (0) or close (1000). A
  result already sent always commits. The gateway may have claimed another task
  in the meantime; it releases a task the worker never answered at once, so
  nothing waits out a lease.
- **An operator stopping the gateway** signals it: the first `SIGTERM`/`SIGINT`
  drains, finishing the task in hand before exiting 0 (a WebSocket gateway
  closes every connection normally and refuses new ones meanwhile); a second
  stops at once. A library that spawned the gateway and is itself signaled
  should not forward the signal: it drains as above, and the gateway follows.

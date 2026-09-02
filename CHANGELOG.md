# Changelog

All notable changes to this project are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.9.0] - 2026-09-02

Go module `v1.9.0`. No PostgreSQL schema changes. The Python client is
versioned independently and advances to `0.12.3`; the JavaScript client is
unchanged.

### Added

- **Python client 0.12.3 lifecycle.** Async EntroQ clients support `aclose()`
  and `async with`; the JSON client now exposes cleanup for its HTTP connection
  pool.
- **Kubernetes mesh benchmark.** A pinned, disposable k3d harness compares raw
  direct HTTP, OPA-authorized direct HTTP, and one- or two-hop EntroQ mesh paths
  at a fixed offered rate across memory, Redis, and PostgreSQL backends. It
  records validation, topology, resource, authorization, and eqlink telemetry
  while keeping raw run artifacts out of the repository.
- **Configurable chart authorization.** The EntroQ Helm chart can explicitly
  omit the OPA sidecar and OPA-only resources for trusted-network deployments,
  or select a custom OPA endpoint and data path.

### Changed

- **Breaking: JWT authentication moves from OPA into the EntroQ service.**
  Services configured with OPA authorization now also use `--authn=jwt` plus
  issuer, audience, and JWKS flags. EntroQ verifies the credential and sends OPA
  an explicit `input.principal`; raw bearer tokens no longer cross the OPA
  boundary. Custom `entroq.user` policy must read the verified principal or its
  verified claims. The protobuf and client bearer-token configuration are
  unchanged. See `pkg/authz/opadata/OPA_AUTHZ.md` for migration details.
- **Authentication failures have transport-specific status.** Invalid
  credentials return `Unauthenticated`, while an unavailable JWKS source returns
  `Unavailable`; OPA authorization denials remain `PermissionDenied`.

### Fixed

- **eqlink data-path metrics.** The `send`, `recv`, and combined `run` commands
  now attach their Prometheus meter provider to the async sender and receiver,
  so deployed sidecars expose handled, error, inflight, and duration metrics.
- **gRPC keepalive compatibility.** The server permits the client's 30-second
  transport keepalive interval, preventing long-held claim streams from being
  disconnected by the default five-minute enforcement policy.

### Security

- **Verified JWT caching is bounded and authorization-independent.** Successful
  verification is cached by complete-token digest until the earlier of a short
  configured TTL or token expiration, with a bounded LRU. Invalid credentials
  and authorization decisions are never cached, unknown signing-key refreshes
  are rate-limited, and OPA policy changes remain immediately effective.

## [1.8.2] - 2026-08-31

Go module `v1.8.2`. No PostgreSQL schema or client version changes.

### Added

- **Experimental SQLite service CLI.** `eqsqlite serve` exposes an embedded
  SQLite database over the same gRPC, HTTP/JSON, authorization, health, and
  Prometheus endpoints as the other backend services.

### Changed

- **Backend service bootstrap is shared.** `eqmem`, `eqpg`, `eqredis`, and
  `eqsqlite` use one internal transport shell for authorization, telemetry,
  gRPC/HTTP listeners, health registration, and coordinated shutdown.

## [1.8.1] - 2026-08-28

Go module `v1.8.1`. No PostgreSQL schema or client version changes.

### Added

- **Experimental SQLite backend.** `pkg/backend/eqsqlite` provides a persistent,
  embedded `entroq.Backend` using WAL mode and serialized transactional writes.
  Its Go API, schema, and on-disk format are experimental and may change or be
  removed without a migration path.
- **Cross-backend performance harness.** Public-API benchmarks now compare the
  memory, journal, SQLite, PostgreSQL, and Redis backends directly and through
  gRPC, with a MapReduce workload for observing queue statistics under load.

### Fixed

- **eqmem ready-task claims.** Availability time is sampled while holding the
  queue lock, closing a small contention window that could transiently report
  no ready task when one had just become eligible.

## [1.8.0] - 2026-07-21

### Added

- **Worker claim limit.** Go workers accept `WithMaxClaims(N)`. Claim `N` may
  run; a later claim is moved to the configured error queue before handler
  construction or payload decoding. Zero keeps the existing unlimited behavior.
- **Worker slot telemetry.** `worker.WithMeterProvider` emits current slot counts
  and longest current state duration through `entroq.worker.slots` and
  `entroq.worker.state.max_duration`, labeled `idle` or `busy`. `eqlink run` and
  `eqlink recv` expose these through their existing Prometheus endpoint.
- **Document garbage collection.** A `/gc=<timestamp>` component in a doc
  primary key opts its complete `(namespace, primary key)` group into the same
  always-on backend GC used by task queues. Collection is all-or-nothing and
  skips any group with a live claim. This uses the ordinary doc API and requires
  no storage-schema change.

### Changed

- **GC follows the worker contract.** Go backends now share one bounded
  collector that lists queues and doc keys, then uses `TryClaim`/`ClaimDocs` and
  ordinary atomic deletes. PostgreSQL's existing GC procedures remain available
  for direct-PG client compatibility, but the Go backend no longer depends on
  them.

### Fixed

- **Redis exact doc-key queries.** `Docs` now bounds the complete primary-key
  prefix in the lexicographic index, so `KeyExact` returns docs whose index
  entries continue with secondary keys and IDs.

---

## [1.7.1] - 2026-07-16

Go module `v1.7.1`. A **breaking** release that is also a **necessary security
update**. `1.7.0` is skipped deliberately (see Changed).

- **Schema change (eqpg: 1.6.0 → 1.7.1).** Existing PostgreSQL deployments must
  run `eqpg schema upgrade` (or re-apply `schema.sql`) before starting the
  upgraded service — `initDB` refuses to open on a version mismatch rather than
  migrating silently. The migration is idempotent and client-transparent (it
  changes only byte-ordering, not data), but it rewrites the `tasks`/`docs`
  tables and rebuilds their indexes, so **plan a maintenance window on large
  deployments**. eqmem and eqredis need no migration.
- **The service wire is unchanged.** `api/entroq.proto` is byte-for-byte
  identical to 1.6.3, so a standard gRPC client or the connect/JSON gateway keeps
  working against the upgraded service with no changes. The breaking changes are
  in the Go `worker`/`entroq` APIs and the (new, experimental) `eqlink work`
  gateway — not in the queue protocol.
- **Older releases are retired.** Prior 1.6.x releases and their Docker images
  are deprecated in favor of 1.7.1 for the security fixes below.

### Security

- **Queue integrity: a modify can no longer misdeclare a task's queue.** The
  queue is now part of the modify key across all backends, checked on every
  change/delete/depend, with the true source queue carried through the gRPC
  change path. Previously an operation could name a queue different from the
  task's own and sidestep queue-scoped authorization; that hole is closed
  (eqmem, eqpg, eqredis), and the service fails closed on empty targets.
- **Authorization fails closed.** OPA denies requests naming an empty queue or
  namespace; the core rejects writes to an empty queue or namespace; document
  operations are authorized in place and cross-namespace doc changes are
  rejected.
- **Arrival-time handling.** Far-past arrival times normalize to now as a
  backend contract, removing an inconsistency in deferred/at handling.
- **Dependency updates.** `golang.org/x/crypto` 0.50→0.54 and `golang.org/x/net`
  0.53→0.57 (the service's TLS/HTTP-2 transport) clear the outstanding
  advisories; `esbuild` is pinned in the JS client's build tooling.

### Added

- **Work gateway (`eqlink work`, package `workgateway`): run an EntroQ worker in
  any language** over a small newline-delimited JSON protocol whose payloads are
  the protojson of the api protobufs. Opt-in phases (takeDocs / work / success /
  dependency), an `ack` shorthand for consuming the claimed task, a one-way error
  channel with a shared exit taxonomy (process exit codes and WebSocket close
  codes), and a supervision loop that rides out a restarting or relocating
  backend (`--entroq-timeout`) transparently to the client. See
  `docs/workgateway-protocol.md` and `examples/workgateway/`.
- **`entroq.IsUnavailable`** classifies a temporarily unreachable backend (the
  gRPC backend translates `codes.Unavailable`), so callers can retry a routine
  restart/relocation without inspecting transport specifics.
- **`DependencyError.Implicates`** for task-scoped dependency-failure checks.

### Changed

- **Breaking: the `worker` API is reworked.** `DoModify` returns a fluently-built
  `Result` with `OnSuccess` (post-commit) and `OnDependency` (dependency-race)
  hooks, replacing `WithDependencyHandler`; `Retry`/`Move`/`Fatal` are structured
  errors with fluent options; handlers are built fresh per task.
- **Breaking: the modify API is fluent-only.** `Put` for docs, the queue rides on
  the identifier, and the older argument styles are gone.
- **Breaking: the `eqlink work` gateway protocol** is the full register / take /
  work / commit contract described in `docs/workgateway-protocol.md` (this
  surface is new and experimental).
- **eqpg schema policy loosened** to permit a client-transparent, idempotent
  non-additive migration within a minor (see `SchemaVersion`). `1.7.0` is skipped
  because it named a queue-array-only schema that existed only on an unreleased
  branch; reusing it would let such a database skip the collation migration.

### Fixed

- **eqgrpc:** `Claim` is a single blocking RPC (the client retry loop is gone),
  and a committed claim is no longer lost to a client retry cancel.
- **eqpg:** byte-order (`COLLATE "C"`) key/queue/id columns so ranges and prefix
  scans match the other backends; `LIKE` metacharacters escaped in SQL prefix
  matching; a test-service backend-loop leak.
- **eqmem:** all modify failure classes are reported in one `DependencyError`;
  old journals that omit an op's queue still replay.
- **workgateway:** a change carries full desired state (not a preserve-on-absent
  delta); a namespace is required on doc delete/depend; reply types are
  validated; a dropped client connection is a clean stop.

## [1.6.3] - 2026-07-08

Go module `v1.6.3`. A rename/refactor release for the cross-instance task handoff
worker and its `eqlink` command. No schema change (schema version stays 1.6.0)
and no client changes.

### Changed

- **The `pullworker` package is renamed to `handoffworker`.** The worker is
  direction-neutral: it claims from a source instance and delivers to a
  destination exactly once, so it is now named for the operation rather than for
  one deployment vantage. `pullHandler` becomes `handoffHandler`, and the default
  graveyard-queue helper is no longer exported.
- **`eqlink push` and `eqlink pull` are replaced by a single `eqlink handoff`.**
  Direction is expressed by explicit `--from`/`--to` endpoints, each with its own
  TLS and bearer-token flags, instead of two mirror-image subcommands. There are
  no external consumers and the handoff service behavior is unchanged, so this
  ships as a patch.

## [1.6.2] - 2026-07-08

Go module `v1.6.2`. A worker-framework bug-fix release. No schema change (schema
version stays 1.6.0) and no client changes.

### Fixed

- **Worker handlers are built fresh per task, not once per `Run`.** Per-task
  handler state was accidentally shared across tasks in a `Run` loop, which let a
  stateful handler act on a previous task's leftover state. `makeHandler` now
  runs per task, isolating state by construction; deliberate cross-task state
  belongs in the handler's closure.

### Changed

- **The cross-instance delivery worker delivers during the work phase.** It now
  performs its remote delivery in `DoWork` (renewal-protected and retryable) and
  finalizes the source delete in `Finish`, rather than doing everything after
  renewal had stopped, so a transient delivery failure retries instead of
  crashing the worker.

## [1.6.1] - 2026-07-06

Go module `v1.6.1`. A bug-fix and performance release for the Redis and
PostgreSQL backends. No schema change (schema version stays 1.6.0) and no new
public API. Python client `0.12.1`.

### Fixed

- **PostgreSQL claims longer than ~35 minutes failed.** `eqpg` (and the Python
  direct-PostgreSQL client) formatted a claim's TTL as an all-microseconds
  interval literal, which PostgreSQL rejects once the field exceeds int32
  (SQLSTATE 22015), so any task or doc claim beyond ~35.8 minutes errored before
  it ran, while `eqmem` and `eqredis` handled it fine. Durations are now split
  into whole seconds plus a sub-second remainder; any duration works.
- **`eqredis` claimant-filtered `Tasks` could return too few results, even
  zero.** The `Limit` was applied before the claimant filter, so a listing could
  come back short (or empty) while matching tasks sat behind tasks claimed by
  other workers. `Limit` now bounds matching results, `min(Limit, #matching)`,
  consistent with `eqpg` and `eqmem`. Claimant-free limited listings are also
  ~9x faster on large queues (the limit is pushed into the range read instead of
  scanning the whole queue). The `Claimant`/`Limit` contract is now documented on
  `TasksQuery`.

### Changed

- **`eqredis` claims run as a single atomic Lua script** rather than a
  WATCH/MULTI optimistic-retry loop, collapsing a claim to one round-trip
  (~3x faster, with ~3-5x lower latency under concurrent claimers). The random
  selection among the most-overdue tasks, EntroQ's anti-starvation guarantee, is
  preserved and now covered by a cross-backend conformance test.

## [1.6.0] - 2026-07-04

Go module `v1.6.0`. Garbage collection becomes a first-class, always-on property
of every backend, superseding the server-side GC layer introduced in 1.5.0. If
you can write a `gc=` queue, it gets collected -- whether you run a server or
talk to a backend directly. Requires an `eqpg schema init` (adds the
`gc_activation` / `gc_queues` / `gc_collect` functions and a partial index;
schema version 1.2.0 -> 1.6.0).

### Changed

- **GC now runs inside each backend, always on.** `eqpg`, `eqredis`, and `eqmem`
  each start a background collector when opened and stop it on close. There is no
  GC configuration: the interval and batch are internal to each backend (tuned to
  its storage engine). A direct-to-PostgreSQL client now collects `gc=` queues on
  its own, exactly as a server does -- no side process, no flags.
- **`gc=` timestamps are parsed strictly.** A `gc=` value is empty/`0` (always
  active), Unix seconds, or strict `RFC3339Nano` (a `T` separator and an explicit
  `Z` or `+hh:mm` offset -- what Go's `time.Parse`, JavaScript's `toISOString`,
  and a timezone-aware Python `isoformat` emit). Non-strict forms (space
  separator, bare date, missing zone) are treated as malformed by every backend,
  so behavior no longer diverges between them.
- **Malformed `gc=` queues are surfaced, not silently ignored.** A queue that
  opts into GC but whose timestamp will not parse is never collected (as before),
  but each sweep now logs it and increments `entroq_gc_errors_total{kind=malformed}`
  -- so a fat-fingered `gc=` value shows up loudly instead of quietly piling up.
- **eqpg GC is a two-function protocol.** `entroq.gc_queues()` enumerates `gc=`
  queues with their activation time (`NULL` = malformed); `entroq.gc_collect(queues,
  activations, limit)` deletes a bounded batch (`FOR UPDATE SKIP LOCKED`) from the
  supplied queues whose activation has passed, returning per-queue counts. Parsing
  lives only in `gc_activation`; collection is grammar-agnostic and clock-correct
  (due-ness is evaluated against the database clock). Replaces the interim
  `gc_due` + single `gc_collect` scan.
- **GC telemetry moved into the backends.** The `entroq_gc_deleted_total`,
  `entroq_gc_errors_total`, and `entroq_gc_sweep_duration_seconds` metrics (and
  their Grafana panels) are preserved, now emitted by each backend's own collector
  under its meter scope (`entroq.pg`, `entroq.mem`, `entroq.redis`) with the same
  names, so one dashboard still covers all backends. `eqredis` gained a
  `WithMeterProvider` option to match `eqpg` and `eqmem`.
- **Python: the worker carries GC for direct-PostgreSQL clients.** An
  `EntroQWorker` wired to the `entroq.pg` client drives the `gc_queues` +
  `gc_collect` loop on a background task for as long as it runs -- invisibly, no
  config, no parsing (the SQL decides everything). Workers talking to a Go server
  over HTTP/gRPC do not (the server GCs itself); the worker gates on whether its
  client exposes `gc_queues`.

### Removed

- **Server GC flags `--no_gc` and `--gc_interval`** (`eqpg`/`eqmem`/`eqredis
  serve`). GC is always on and self-tuning; there is nothing to toggle.
- **`eqlink gc`** and the **`--run-gc`** flags on `eqlink run`/`pull`/`push`. The
  destination backend collects gc= queues (including `pull`/`push` dedup
  tombstones, which keep their `gc=0` naming) with no sidecar loop.
- **`pkg/gc`** and `eqsvcgrpc`'s `WithGC`/`WithGCInterval`. The reusable
  claim-loop collector is gone; collection is a backend responsibility now.

---

## [1.5.0] - 2026-07-02

Go module `v1.5.0`. Built-in, best-effort garbage collection: EntroQ servers now
reap queues that opt in by name, so fault-tolerance recipes no longer need a side
process for cleanup.

### Added

- **Server-side garbage collection.** `eqpg`, `eqmem`, and `eqredis serve` run a
  background loop that drains any queue whose name carries a `/gc=<timestamp>`
  component once that time passes, using ordinary claim/delete so it never removes
  a task a worker holds. On by default; `--no_gc` opts out and `--gc_interval`
  tunes the scan period (default 1m). Emits OTel metrics (`entroq_gc_deleted_total`,
  `entroq_gc_errors_total`, `entroq_gc_sweep_duration_seconds`) with Grafana panels.
  GC lives in the server: a client that talks directly to a backend (notably the
  direct-to-PostgreSQL, many-clients model with no server in front) gets no
  built-in GC and must run its own collector -- see the `eqpg` package docs.
- **`pkg/gc` and `queues.GCActivation`.** A reusable, matcher-scoped collector and
  the `gc=` naming convention (empty/`0`/Unix-seconds/RFC3339), usable standalone
  as well as embedded in a server.

### Changed

- **`eqlink run --run-gc` now defaults off.** The server collects garbage, so the
  sidecar no longer needs its own loop. Enable it only against a server run with
  `--no_gc`.
- **Async response queues use `gc=` instead of `exp=`.** One naming convention. The
  sender bakes its clock-skew grace into the `gc=` timestamp (`--response_grace`),
  and the collector simply obeys the timestamp it sees.
- **`eqlink pull`/`push` tombstones are reaped by the destination server's GC.**
  The dedup tombstone queue now carries a `gc=0` marker, so the built-in server GC
  collects crash orphans once their TTL elapses. The bespoke sidecar reaper is
  replaced by an opt-in `--run-gc` on each command: leave it off when the
  destination server runs GC (the default), set it when it does not (e.g. a
  direct-to-PostgreSQL destination).

### Removed

- **GC no longer recognizes the `exp=` alias.** Response queues left in flight by a
  pre-1.5.0 sender will not be collected by the new GC; they are ephemeral, so this
  is a one-time, low-impact gap during upgrade.

---

## [1.4.1] - 2026-07-01

Go module `v1.4.1`. Secures the `eqlink pull`/`push` local connection (TLS + token).

### Fixed

- **`eqlink pull` and `push` can now secure the local connection.** The local
  (`--entroq`) connection previously ignored transport security and auth; it now
  honors `--cert`/`--key`/`--ca` (TLS) and `--authz-token-file` (bearer token,
  reloaded on rotation as `run` already does). Fixes #73. The token-reload logic
  is now shared across `run`, `pull`, and `push` via a new internal helper.
- **Remote TLS inherits the local `--cert`/`--key`/`--ca`** when a `pull`/`push`
  invocation sets none of its `--source-*`/`--dest-*` flags (logged). A single
  trust domain then needs the certs given only once; distinct trust domains still
  set their own per-connection flags. This also removes a footgun: an unset
  remote is no longer silently insecure.

---

## [1.4.0] - 2026-07-01

Go module `v1.4.0`. Adds `eqlink push`, the source-side mirror of `pull`.

### Added

- **`eqlink push`.** The mirror of `eqlink pull`: claims from a local queue and
  delivers into a remote instance's inbox, exactly once, over the same
  `pullworker` engine. Runs next to the source (leaf), for hub-and-spoke fan-in.
  The dedup tombstone lives on the remote destination, so its cleanup crosses the
  wire; `--source-name` must be unique per source instance to avoid collisions on
  the shared destination.

---

## [1.3.0] - 2026-07-01

Go module `v1.3.0`. Adds exactly-once cross-instance task handoff (`eqlink pull`).

### Added

- **`eqlink pull` and `pkg/workers/pullworker`.** Link two EntroQ instances
  directly: claim tasks from a queue on a remote source instance and deliver
  them into a local inbox, exactly once in effect. Each delivery atomically
  inserts the inbox task and a value-stripped dedup tombstone keyed by a
  deterministic transfer ID, then deletes the source task; a crash that
  re-delivers collides on the tombstone, so no duplicate inbox task is produced.
  The happy path deletes its own tombstone immediately, and a reaper sweeps
  crash orphans once their TTL elapses. Run it next to the destination instance,
  so only the claim from the source crosses the wire.
- **`worker.NoWork`.** A standard no-op work function for finalize-only workers
  (those whose whole job is moving tasks between queues), used with
  `WithDoWork` alongside a `WithFinish`.

### Removed

- **At-least-once `Forwarder` and `eqlink forward`.** Superseded by `eqlink
  pull`'s exactly-once handoff. A simple at-least-once "send a request and delete
  the task" worker is a few lines anyone can write with the `worker` package.

---

## [1.2.1] - 2026-06-15

Go module `v1.2.1`. Client packages: TypeScript `entroq` 0.11.0 (drops the
direct-PG client), Python `entroq` 0.10.1.

### Added

- **`eqc work` shell worker.** Claims from one or more input queues, writes the
  input task value to a local command's stdin, parses stdout as JSONL output
  tasks for a single `--out-queue`, and finalizes with one atomic `Modify`.
  Supports delayed output tasks via `--in`, unlimited retries by default, and
  cron-like requeueing via `--recur-in`.

### Fixed

- **gRPC task changes now preserve requested arrival time (`At`).** The gRPC
  service reconstructed task changes with bare `Changing(t)`, which resets `At`
  to the default release/immediate value. Changes sent over the gRPC protocol
  now apply the wire `At` explicitly, fixing delayed task changes such as
  retry/defer modifications made by gRPC clients.
- **JSON/Connect error translation.** `eqsvcgrpc.QSvc` returns
  `google.golang.org/grpc/status` errors, which ConnectRPC did not recognize:
  served over JSON they collapsed to HTTP 500 (`UNKNOWN`) with the real status
  stringified into the message and all details dropped. `eqsvcjson` now
  translates grpc/status errors into `connect.Error`, preserving the code and
  re-attaching `ModifyDep`/`AuthzDep` details. This affected **every** coded
  error over the JSON endpoint — dependency errors and authz denials alike.
- **JSON responses now emit zero-valued fields** (`version:0`, `atMs:0`,
  `claims`, `attempt`, …). Vanguard was relaying the in-process Connect
  backend's JSON, whose codec omits defaults; the transcoder now targets the
  backend in proto and re-marshals responses with its own `EmitUnpopulated`
  codec. Thin clients no longer have to infer that a missing field means zero
  (which had made a version-0 task's `version` undefined in the TS client).

### Changed

- **Dependency errors over JSON are now `409 Conflict`** (gRPC `Aborted`
  semantics: an optimistic-concurrency conflict to retry with fresh versions),
  not a cacheable 404 or an opaque 500. Details are carried as a flat
  `ModifyDep` list. The Python and TypeScript clients detect the dependency
  error by the 409 status (404 still accepted for tolerance) and parse the
  failed IDs; the TS client now throws a typed `EntroQDependencyError`.
- **`eqgrpc` accepts both `NotFound` and `Aborted`** as the dependency-error
  code, so the gRPC service can switch from `NotFound` to `Aborted` at a future
  minor version with no client change. The service still emits `NotFound`.

### Removed

- **The JavaScript direct-to-PostgreSQL client** (`EntroQPG`, `clients/js/src/pg`)
  and its `pg` dependency. The direct-PG path was incomplete (no doc support);
  use the HTTP `EntroQClient`. Direct doc-store interaction is demonstrated in
  the Python client.
- `DOCS_TODO.md` (stale planning scratch).

### Docs

- Rewrote the README client quick-starts to match the shipped APIs: the async
  Python worker (`EntroQWorker(eq, *queues)` + `Modification`-returning handler),
  the `entroq` npm package name with modify-request handler returns, and the Go
  `WithDoModify` / `worker.Watching` worker. Corrected the Python install path
  (`#subdirectory=clients/py`) and dropped the removed `python -m entroq` CLI.
- Added a runnable Python worker example (`clients/py/examples/worker/`) that is
  exercised end-to-end by `clients/py/tests/test_example_worker.py` against an
  `eqmem` subprocess — the Python analog of Go's testable examples.

## [1.2.0] - 2026-06-06

### Added

- **`entroq.Renew`** with `RenewConfig` / `RenewOption` / `RenewResponse`: atomically
  renews tasks and docs in a single `Modify` call. Replaces the former piecemeal
  renewal helpers as the canonical renewal primitive.

- **`Handler[T].TakeDocs`**: handlers can now declare which doc namespaces/keys they
  want acquired before work begins. The worker fetches and locks them in sorted order
  (deadlock-safe) and passes them via `DoWhileRenewing`.

- **`eqk8s` namespace policy** (`OPANamespacePolicy`, `AllowedCallers`): the operator
  now pushes per-namespace caller policy into the OPA mesh document. CRD
  (`EntroQQueue`) and controller updated accordingly; OPA tests now load each auth
  provider (entroq/OIDC, k8s) in isolation to prevent package conflicts.

- **Python doc types**: `DocID`, `DocData`, `DocChange`, `Doc` with `as_id()` /
  `as_change()` helpers; `docs()`, `claim_docs()`, `modify_docs()` on `EntroQBase`
  and the pg implementation. Worker gains `Handler` ABC, `RetryError`, `MoveError`,
  `DocClaim`; `Modification` and `ModifyResult` exported.

- **TypeScript doc types and worker**: all doc types added; `ModifyRequest` /
  `ModifyResponse` extended with doc fields; `EntroQDocClientInterface` with
  `docs()`, `claimDocs()`, `namespaceStats()`.

### Changed

- **Worker renewal model** (`pkg/worker`): `DoWhileRenewing` replaces the former
  `DoWithRenew` / `DoWithRenewAll` entry points. The worker now runs a single
  atomic renewal loop covering both tasks and docs. `FinalizeRenew` returns
  `*RenewResponse` (tasks + docs) instead of `[]Task`.

- **`eqmem` doc namespace storage**: btree dual-index replaces `sync.Map`, giving
  O(log n + k) range operations. Clone-on-read provides lock-free scans; also fixes
  a `NamespaceStats` data race introduced by the btree switch.

- **`eqc rm`**: quieter output, more consistent return values.

### Fixed

- **gRPC `DependencyError` doc encoding**: `DocInserts` was missing from
  `DependencyError` on both the server (encoding) and client (decoding) sides.
  Doc collision errors now include `ActionType_INSERT` as expected.

- **Async Python tests**: missing leading slash on queue paths in async test helpers.

### Breaking

- **`DoWithRenew` / `DoWithRenewAll` / `ClaimWithRenew` / `ClaimRenew` removed**
  from the public Go API. Use `entroq.Renew` directly for custom renewal loops, or
  build workers via `pkg/worker` (`Handler[T]` + `Worker.Run`).

- **`DocQuery.KeyExact`** replaces the `key_equals` proto field (field was new in
  1.1.0 with no known external callers).

- **`DoWork` function type** updated: the second argument is now `*RenewResponse`
  (carries renewed tasks and docs) instead of `[]Task`.

---

## [1.1.0] - 2026-05-22

### Added

- **`NamespaceStats`** (`EntroQ.NamespaceStats`, `Backend.NamespaceStats`): new
  method returning per-namespace doc statistics (`Size`, `Claimed`), with
  prefix/exact filtering and limit, analogous to `QueueStats` for task queues.
  Implemented in all backends: eqpg (index-only scan), eqmem (lock-free
  `sync.Map` range), eqredis (new `{eq}:ns` registry set + `{eq}:nsclaimed:{ns}`
  ZSET), and eqgrpc (new `NamespaceStats` RPC).

- **`eqc ns` command** (alias `namespaces`): lists doc namespaces with size and
  claimed counts. Mirrors `eqc qs` / `eqc stats` for task queues.

- **`MatchQuery` type** (replaces `QueuesQuery`): the prefix/exact/limit query
  struct is now named `MatchQuery` to reflect its use in both queue and namespace
  listing. `QueuesQuery` remains as a type alias. `WithLimit` replaces
  `LimitQueues`; the old name is kept as a `var` alias.

- **`{eq}:qsclaimed:{name}` ZSET** (eqredis): replaces the `{eq}:inflight:{name}`
  SET for claimed task tracking. Score is `AtMs` (claim expiry), so
  `ZCOUNT >now` gives an exact current claimed count without a GC pass.
  `Claimed` and `Future` in `QueueStats` are now exact rather than approximate.

- **`{eq}:nsclaimed:{ns}` ZSET and `{eq}:ns` SET** (eqredis): parallel
  structures for doc namespaces, enabling exact claimed counts and efficient
  namespace enumeration without a full keyspace `SCAN`.

- **`NamespacesRequest` / `NamespacesResponse` / `NamespaceStat` proto messages**
  and `NamespaceStats` RPC added to the gRPC API.

- **`at_ms` field on `DocData` proto message**: doc changes now carry the
  requested arrival/claim-expiry time over gRPC. Previously, doc changes with a
  future `At` (e.g. renewals via `Modify`) silently dropped the timestamp,
  making claim-by-modify impossible over gRPC.

### Fixed

- **Claimant cleared after `Modify`** (eqpg): a task change without an explicit
  future `At` was not reliably clearing the claimant due to client/server clock
  skew — Go's `time.Now()` arrived at the database fractionally ahead of
  `v_now`, causing the "renewal" branch to fire and preserve the claimant.
  `Changing()` now sends zero time as a sentinel; the schema treats any `at`
  older than one year as "use `v_now`" rather than comparing against the exact
  epoch value.

- **Doc claimant always set on `Modify`** (all backends): doc inserts and
  content-only changes unconditionally set `Claimant` to the modifier's ID.
  Fixed to match task semantics: claimant is set only when the new `At` is
  strictly in the future; past or zero `At` releases the claim.

- **`Doc.Copy()` with nil content** produced `[]byte{}` (empty non-nil slice),
  which serializes as invalid JSON and caused `log.Fatalf` in the eqmem
  journaling path. Now leaves `Content` nil when there is no content.

- **`_modify_docs` `IS NULL` check** (eqpg): the doc-change claimant condition
  used `IS NULL` to detect "no new arrival time", but Go sends zero time as the
  Go epoch (`0001-01-01`), not SQL NULL. Fixed to use the same `> v_now` /
  one-year threshold pattern as `_modify_arrays`.

### Changed

- **Schema version `1.0.1` → `1.1.0`** (eqpg): four columns made `NOT NULL`:
  `tasks.claimant`, `tasks.created`, `docs.claimant`, `docs.at`. All default
  to `''` or `now()`. Idempotent migration blocks handle existing databases.
  `docs.at` is now `NOT NULL DEFAULT now()`, aligning its semantics with
  `tasks.at` (both represent claim expiry; `at > now` with non-empty claimant
  means currently held).

- **eqredis GC simplified**: the per-queue inflight-expiry loop is removed.
  GC now only removes empty queues from `{eq}:qs` and empty namespaces from
  `{eq}:ns`. Claimed-entry cleanup is handled atomically in claim/modify/delete
  paths or by the ZSET score semantics.

- **`queueMatches` → `matchesQuery`** (eqmem): renamed to reflect use in both
  queue and namespace filtering.

---

## [1.0.0] - 2026-05-18

### Added

- **Kubernetes mesh operator** (`cmd/eqk8s`): new controller-runtime operator
  that watches `EntroQQueue` and `EntroQIdentity` CRDs across all namespaces,
  builds an OPA authorization document from them, and pushes it to OPA via the
  data API on every reconcile. Also hosts a validating admission webhook that
  rejects malformed CRDs before they reach etcd.

- **`EntroQQueue` CRD**: declares which queues a service exposes and which
  callers may enqueue to them, expressed as label-set predicates
  (`allowedCallers`). AND semantics within one entry; OR semantics across
  entries. Patterns support `Exact`, `Prefix`, and `Glob` match types.

- **`EntroQIdentity` CRD**: maps a Kubernetes service account to a set of mesh
  label claims. The operator resolves these claims at authorization time, so
  the OPA policy never inspects raw JWT fields directly.

- **Helm chart** (`charts/entroq`): single-command install of the operator and
  an EntroQ server. Four backend modes: `memory` (ephemeral), `journal`
  (StatefulSet + PVC), `postgres`, and `redis`. Passwords come from Kubernetes
  Secrets; an `existingSecret` pattern is supported for production.

- **eqlink host-header routing**: services call
  `http://svc-name.localhost:8080/path`; eqlink reads the `Host` header,
  strips the domain suffix, and routes to the correct queue. Configurable via
  `--domain-suffix` (default `.localhost`; use `.eq.local` with a CoreDNS
  wildcard in-cluster). `--namespace` sets the caller's namespace for
  cross-namespace calls via `http://ns.svc.eq.local:8080/path`.

- **eqlink audit logging** (`--audit-log`): structured JSON events on stderr
  via slog. Events: `request_enqueued` (sender side), `request_handled`
  (receiver side), `response_received` (sender side). Correlation key is the
  per-request `response_queue`. Payload content is never logged. Production
  path: stderr → Promtail/Alloy → Loki.

- **eqlink credential rotation**: the token file (`--token-file`) is re-read
  on a 5-minute poll interval and immediately on SIGHUP, enabling credential
  rotation without restart.

- **eqlink Dockerfile**: the eqlink sidecar now has its own image
  (`entroq-operator` repo, `eqlink` binary), suitable for use as an init or
  sidecar container.

- **K8s OPA provider** (`pkg/eqk8s`): parses `system:serviceaccount:ns:name`
  from the JWT `sub` field, auto-grants `ALL` on the service's own queue
  prefix, and resolves label claims from the OPA mesh document.

- **OPA k8s Rego policy and tests** (`pkg/authz/opadata/conf/providers/k8s`):
  complete mesh authorization policy with a full test suite covering
  cross-namespace calls, response-queue grants, and label predicate evaluation.

- **greetings-demo** (`examples/greetings-demo`): end-to-end example of three
  Python services communicating via the queue mesh on Kubernetes, including
  manifests, `EntroQQueue`/`EntroQIdentity` declarations, and a step-by-step
  README.

### Fixed

- **gRPC client claim retry loop** (`pkg/backend/eqgrpc`): a `WithTimeout`
  context expiry was treated as a terminal error rather than a retriable
  deadline, causing workers to exit silently after a 2-minute idle wait.

- **eqlink `send` request timeout**: the default was `0` due to `init()`
  ordering shadowing `run`'s 30-second default, causing instant claim failures
  that disrupted the shared gRPC connection and killed the inbox receiver.

- **Queue name leading slash** (`pkg/async`): `queueFromHost` produced
  `ns/svc` instead of `/ns/svc`, causing silent OPA authorization failures for
  all mesh calls.

- **OPA k8s permissions response-queue grant**: callers permitted on
  `X/inbox` now also receive a `CLAIM` grant on the `X/response/` prefix so
  they can receive replies.

- **Content-Length forwarding** (`cmd/eqlink`): forwarding `Content-Length`
  from the original request caused `IncompleteRead` after JSON round-trip
  through the envelope. It is now stripped (treated as hop-by-hop) and
  recalculated by the response writer from the actual body.

- **errgroup context propagation** (`cmd/eqlink`): receiver, GC, and SIGHUP
  goroutines were started with independent contexts; failures did not propagate
  to siblings. All now derive from the root group context.

### Changed

- **Image names** standardized: `entroq-mem`, `entroq-pg`, `entroq-redis`,
  `entroq-operator`. Previously inconsistent across Dockerfiles and k8s
  manifests.

---

## [1.0.0-rc3] - 2026-04-16

### Added

- **Redis backend** (`pkg/backend/eqredis`): new production-quality backend
  using Redis WATCH/MULTI/EXEC for optimistic locking. Supports all task and
  doc operations, including claims, renewals, modifications, and dependencies.
  All keys use the `{eq}` hash tag, ensuring cluster-mode compatibility by
  pinning all data to a single hash slot (see package doc for the tradeoff).

- **Doc insert collision detection**: inserting a doc with an explicit ID now
  returns `DependencyError.DocInserts` if that ID already exists, matching the
  behavior of task inserts. Previously eqredis silently overwrote and eqpg
  misrouted the error through the task error parser. eqmem was already correct.

- **`WithSkipCollidingDoc`** (`DocOpt`): marks a doc insert as droppable on ID
  collision, analogous to `WithSkipColliding` for task inserts. The `Modify`
  retry loop strips skippable collisions and retries automatically.

- **`WithDocArrivalTime` / `WithDocArrivalTimeBy`** (`DocOpt`): set the `At`
  field on a doc change, enabling renewal or claim-by-ID patterns without
  mutating the `Doc` struct directly.

- **`TryClaimDocByID`**: client-level helper that claims a specific doc by
  namespace and ID, analogous to the `eqc tryclaimid` command for tasks.

- **`DependencyError.HasCollisions`** now covers doc insert collisions
  (`DocInserts`) in addition to task insert collisions (`Inserts`), so the
  `Modify` retry loop handles both uniformly.

- **Release script** (`scripts/tag-release.sh`): pre-flight checks before
  pushing a version tag (clean tree, no `replace` directives, CHANGELOG entry
  present, tag does not already exist).

- **eqmem move**: eqmemsvc became instead `eqmem serve` to mirror eqpg.

- **eqredis**: starting an eqredis instance now has its own command.

### Fixed

- **eqpg doc modify errors**: `_modify_docs` errors were parsed by the task
  error parser, which ignored the `ns` field and misrouted collisions into
  `Changes`. A dedicated `parseModifyDocsError` now correctly populates
  `DocDepends`, `DocDeletes`, `DocChanges`, and `DocInserts`.

- **Claimant preservation on renewal**: a `Change` that pushes `At` into the
  future (renewal) now preserves the existing claimant in all three backends.
  Previously all three backends cleared the claimant unconditionally on any
  `Change`, breaking worker renewal loops.

---

## [1.0.0-rc2] - 2026-04-15

### Fixed

- Removed self-referential `replace` directive from `go.mod` that would break
  `go get github.com/shiblon/entroq` for downstream consumers.

---

## [1.0.0-rc1] - 2026-04-15

First release candidate for 1.0. The 0.x series was exploratory; 1.0 carries
a stability promise: no breaking API changes within the 1.x line, and schema
changes only on minor version bumps and only additively (no data movement).

There is no migration path from 0.x schemas. If you have a 0.x PostgreSQL
deployment, drain all tasks and reinitialize:
`DROP SCHEMA entroq CASCADE`, then run `eqpg schema init`.

This also applies to in-memory journaled deployments. Tasks are generally incompatible in memory and on disk, so a journal replay of v0 journals using v1 code will simply fail.

V0 clients speak a different gRPC wire protocol, as well, so workers will need to be upgraded, regardless of implementation language, to work with a V1 service.

### Added

- **EntroQ Core**
  - **Notifications** now support "the passage of time" as an event. Before only insertions and updates would trigger a notification.
  - **Key-Value document store**: durable documents can be stored alongside tasks and transparently updated in the same transaction as task modifications.
  - **OpenTelemetry / Prometheus metrics**: opt-in instrumentation in
    `eqmemsvc` and `eqpg` service binaries.
  - **Version flag**: all binaries now support `--version` (set at build time
    via `-ldflags`).
- **OPA**
  - **Claimant Spoofing** is now detectable to some extent and authorization can be granted for it (for admin tools).
  - **Full JWKS Support** is now part of the repository. Plug in an standard OIDC and you can manage access.
  - **Full Startup Examples** are now provided as a docker compose setup.
- **CLI Tools**
  - **eqctl** for managing administrative tasks like OPA policy
  - **eqlink** as a sidecar capability for using EntroQ for microservices; make your microservices asynchronous without them needing to know about queueing. Supports mTLS. Also works for connecting two EntroQ instances to one another, forwarding tasks from a local queue to a remote queue.
- **PostgreSQL Enhanced Support**
  - **Stored Procedures** now contain all basic EntroQ functionality, except client-side workers. You can technically just apply the schema and use psql to do task claiming and modification. Works for documents, as well.
  - **NOTIFY/LISTEN** is used as a notification strategy
  - **Schema version tracking and compatibility policy**: `entroq.meta` stores
    the schema version; the backend refuses to start on a version mismatch.
    1.x schema upgrades are always additive and handled by re-running the DDL.
  - **Faster claims** enabled by avoiding ORDER BY random(). Now uses hash bits to bucket first. Orders of magnitude faster on large databases.
  - **Non-blocking statistics** are now implemented as an index-only scan, making dashboards less contentious.
- **Language Support**
  - **HTTP+JSON** is now a first-class citizen. No need for gRPC to speak to the service.
  - **Python** gRPC moved entirely to HTTP_JSON
  - **Python direct to Postgres** is now an option, and permits arbitrary database operations in the same transaction as task modification. Does not scale well, but it is an option.
  - **Typescript/Node.js** now has a full client implementation.
- **Worker Library** (`pkg/workers`): reusable worker implementations for common patterns:
  - **batchworker** -- aggregates tasks and processes them in configurable batches
  - **mapworker** -- generic map operation; input task value is transformed and routed to an outbox
  - **httpworker** -- executes HTTP requests described by task values; response written back as task
  - **fileworker** -- writes task values to files
  - **appendworker** -- appends task values to a stuffedio WAL journal
  - **procworker** -- runs a subprocess described by the task value; stdout/stderr captured in output task
- **EntroQ Client (eqc)**
  - **tryclaimid** allows a claim of a specific task ID for admin operations that want to be fairly safe.

### Changed

- **Task values are JSON** instead of raw bytes. Raw
  bytes are no longer supported.
- **IDs are `text`** (were `uuid`). Accepts UUIDs, ULIDs, or any string up to
  64 characters. Default IDs are now 16-character hex.
- **Schema namespace**: all tables and functions live in the `entroq` schema
  (were in `public`).
- **Schema DDL is now clean**: no ALTER TABLE, no DROP/CREATE pairs, no
  extensions required. Just tables, indexes, types, and functions. There is no upgrade path from 0.x to 1.x. There is a way to upgrade, but it requires help from the developer. Reach out.
- **`eqmemsvc`** journal format updated; periodic snapshot and cleanup flags
  added.
- Postgres schema version bumped to `1.0.0` (was `0.12.0`, which was never released).
- Postgres `pgcrypto` extension dependency (no longer needed; IDs are generated in Go).

---

## [0.9.1] - 2023-02-14

- PostgreSQL backend stabilization: stored-procedure-based claim with
  LISTEN/NOTIFY, fairness improvements, deadlock retry.
- Library and dependency updates.

## [0.8.8] - earlier

- Initial public releases. Exploratory; no stability guarantees.

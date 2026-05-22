# Changelog

All notable changes to this project are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

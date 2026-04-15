# Changelog

All notable changes to this project are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

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
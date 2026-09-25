# Agent guide

Repo-specific guidance for AI coding agents. General contribution norms —
workflow, formatting, testing, releasing, commit style — live in
[CONTRIBUTING.md](CONTRIBUTING.md) and apply to agent-authored changes too. This
file collects the things that are easy to get wrong here.

## Keep the PostgreSQL schema version in lockstep

`pkg/backend/eqpg/schema.sql` is the canonical PostgreSQL schema, applied by
`eqpg schema init` and embedded in the Go backend. Its `schema_version` stamp
must equal `SchemaVersion` in `pkg/backend/eqpg/schema.go`; clients reach the
schema through the Go service rather than executing it directly.

`TestSchemaVersion` in `pkg/backend/eqpg` enforces this invariant.

Between releases the version carries a `-dev` suffix (`1.13.0-dev`), and a
`-dev` database is never current: `eqpg schema upgrade` always re-applies it.
The version alone cannot tell two builds' schemas apart, so `InitSchema` also
records `SchemaDigest`, the SHA-256 of `schema.sql`, and `Open` refuses a
database whose digest differs or is missing. Any edit to `schema.sql` changes
the digest; that is the point. The release drops the suffix, and
`scripts/tag-release.sh` refuses to tag while it is present. The SQLite backend
records a digest the same way; a file at the current version with another
digest came from a development build and is refused.

## The Go worker is the reference implementation

`pkg/worker` defines EntroQ's worker semantics. The Python, JS, and gateway
workers are ports of it, so "parity" means parity **with Go** — not with each
other, and not with whatever a client happens to do today. When a client
diverges, the fix is to change the client.

This holds even when the divergent behavior is documented in the client. A
comment asserting the behavior is evidence that someone wrote it deliberately,
not evidence that it is right: the June 2026 client rewrite documented
"if queue is empty, the claim expires naturally" for a move with no
destination, and that was a task-losing bug in both Python and JS for a year.

Read these before changing any client worker:

- `pkg/worker/worker.go` — the run loop, the phase order, and the error ladder
  (sentinel → dependency → cancellation → exit).
- `Task.RetryOrQuarantine` (`task.go`) — retry and quarantine are **one**
  decision, taken at the moment of failure. A client that re-queues and then
  checks the attempt ceiling on the *next* claim has a bug: an exhausted task
  gets quarantined only if a worker comes back for it, and none may. Quarantine
  also resets the arrival time, so a retry delay cannot leak into a task that is
  waiting to be inspected.
- `worker.DefaultErrQMap` — the default error queue is `<inbox>/err`, and
  `WithErrQMap` computes it per inbox. A move with no destination falls back to
  it; it never becomes a no-op.
- `acquireDocs` — a doc group held by someone else is transient (retry with
  backoff), recorded on the task, not just logged. A group with no docs claims
  normally and returns none; the handler decides what an empty group means.
- `DependencyError` (`entroq.go`) — task and doc failures are separate fields.
  A `ModifyDep` carries either `id` or `doc_id`; a decoder that reads only `id`
  silently discards every doc dependency.

The ports live in `clients/py/src/entroq/worker.py` and
`clients/js/src/worker.ts`. `pkg/workgateway` hosts foreign workers on the Go
worker directly, so it inherits these semantics rather than reimplementing them
— but it must still pass through each run option it means to support.

## Releasing goes through the scripts, off `develop`

Do not hand-roll a release. Releases are cut from `develop` (not `main`, which
is unused), and the tooling exists:

- `scripts/tag-release.sh <version>` — runs pre-flight guards (clean tree, no
  `go.mod` `replace`, a `CHANGELOG.md` entry, and `SchemaVersion` not exceeding
  the tag) then creates and pushes `v<version>`. The schema version advances
  only when the schema changes, so it may lag the module version. Prefer the
  script over a bare `git tag` so the guards run.
- `scripts/build-docker.sh <version> --push` — builds and pushes the service
  images to `ghcr.io`.
- Python/JS clients version and publish independently (`scripts/publish-py.sh`,
  `scripts/publish-js.sh`).

The full checklist is [scripts/RELEASE.md](scripts/RELEASE.md).

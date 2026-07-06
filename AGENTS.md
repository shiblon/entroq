# Agent guide

Repo-specific guidance for AI coding agents. General contribution norms —
workflow, formatting, testing, releasing, commit style — live in
[CONTRIBUTING.md](CONTRIBUTING.md) and apply to agent-authored changes too. This
file collects the things that are easy to get wrong here.

## The two `schema.sql` files must never drift

The PostgreSQL schema exists in two copies:

- `pkg/backend/eqpg/schema.sql` — **canonical** (applied by `eqpg schema init`).
- `clients/py/src/entroq/experimental/pg/schema.sql` — a vendored copy bundled in the Python
  wheel so a pip-only user can prepare a database without a Go toolchain.

Treat the Go file as the source of truth and the Python file as a copy of it.
When you change one, change the other **identically** — they must stay
byte-identical — and keep the schema-version constants in lockstep:

- the schema stamps `schema_version` into `entroq.meta`,
- `pkg/backend/eqpg/schema.go` declares `SchemaVersion`,
- `clients/py/src/entroq/experimental/pg/__init__.py` declares `SCHEMA_VERSION`,

and all three must equal the same value. Each client checks the stamped version
on connect, so a mismatch makes the client refuse to connect, and any schema
divergence makes behavior differ silently between backends. A long, unnoticed
drift here already cost a full resync once; do not restart that habit.

This is enforced by `TestSchemaFilesInSync` in `pkg/backend/eqpg` (runs under
`go test`): it fails if the two files differ or the version constants disagree.
After editing the canonical Go schema, run `make schema-sync` to regenerate the
Python copy, and update `SCHEMA_VERSION` to match if the stamped version changed.

## Releasing goes through the scripts, off `develop`

Do not hand-roll a release. Releases are cut from `develop` (not `main`, which
is unused), and the tooling exists:

- `scripts/tag-release.sh <version>` — runs pre-flight guards (clean tree, no
  `go.mod` `replace`, a `CHANGELOG.md` entry, `SchemaVersion` major.minor
  matching the tag) then creates and pushes `v<version>`. Prefer it over a bare
  `git tag` so the guards run.
- `scripts/build-docker.sh <version> --push` — builds and pushes the service
  images to `ghcr.io`.
- Python/JS clients version and publish independently (`scripts/publish-py.sh`,
  `scripts/publish-js.sh`).

The full checklist is [scripts/RELEASE.md](scripts/RELEASE.md).

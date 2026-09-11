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

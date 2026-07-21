# Release Process

Releases are cut from `develop` and are **git-tag-driven** (see
[CONTRIBUTING.md](../CONTRIBUTING.md#releasing) for the versioning stance). This
document is the operational checklist and the reference for the release scripts.

## Prerequisites

- `gh` CLI authenticated
- `docker` authenticated to `ghcr.io` (for the image push):
  `echo <github-pat> | docker login ghcr.io -u shiblon --password-stdin`
  (the PAT needs the `write:packages` scope)
- Working tree clean on `develop`

## Steps

### 1. Prepare release artifacts on `develop`

```sh
git checkout develop && git pull
```

- Finalize `CHANGELOG.md`: rename `## [Unreleased]` to `## [<version>] - <YYYY-MM-DD>`.
- If the PostgreSQL schema changed, bump `SchemaVersion` in
  `pkg/backend/eqpg/schema.go` **and** the matching `INSERT` in
  `pkg/backend/eqpg/schema.sql` (then `make schema-sync`, and update
  `SCHEMA_VERSION` in the Python client). `tag-release.sh` enforces that
  `SchemaVersion` does not exceed the release tag. It advances only when the
  schema changes, so a schema-unchanged release leaves it alone.
- If the Python client changed, bump its independent version in
  `clients/py/pyproject.toml` (it is not tied to the Go module version).

Commit and push:

```sh
git add -A
git commit -m "chore: prepare release v<version>"
git push origin develop
```

> `main` is not part of the flow. Releases have been cut from `develop` since
> v1.3.0; `main` is unused.

### 2. Tag the release

```sh
./scripts/tag-release.sh <version>    # e.g. 1.6.0 -- no leading 'v'
```

The script runs pre-flight checks (clean tree; no `replace` directives in
`go.mod`; a `CHANGELOG.md` entry for the version; `SchemaVersion` does not
exceed the tag; the tag does not already exist), then creates and pushes
`v<version>`.

### 3. Build and push Docker images

```sh
./scripts/build-docker.sh <version> --push
```

Builds and pushes the `entroq-pg`, `entroq-mem`, `entroq-redis`,
`entroq-operator`, and `entroq-link` images to `ghcr.io` (tagged `<version>`,
`<major>.<minor>`, and `latest` for stable releases).

### 4. Publish clients (as needed)

The Python and JS clients version and publish independently of the Go module:

```sh
PYPI_TOKEN=<token> ./scripts/publish-py.sh <version>    # PyPI
NPM_TOKEN=<token>  ./scripts/publish-js.sh <version>    # npm
```

Both publishes run non-interactively (the JS one inside a Docker container), so
the tokens must not require an interactive 2FA prompt:

- **PyPI:** the very first publish of a new project name needs an **account-scoped**
  token (a project-scoped token can't create a project that doesn't exist yet);
  scope it down to the project afterward.
- **npm:** use a classic **Automation** token, or a granular token with **"Bypass
  2FA"** enabled (and all-packages write for a first publish). A plain "Publish"
  token fails non-interactive uploads with `E403 ... two-factor authentication ...
  required`.

## Version numbering

- **Patch** (`1.x.y+1`): bug fixes, no schema changes, no new public API.
- **Minor** (`1.x+1.0`): new features; may include compatible schema changes.
  When the schema changes, set `SchemaVersion` to this release and existing
  databases upgrade by running `eqpg schema upgrade`. Otherwise leave
  `SchemaVersion` at the release where the schema last changed.
- **Major** (`x+1.0.0`): breaking changes.

# Contributing to EntroQ

Thanks for helping improve EntroQ. This document captures how the project is
developed and released so the process lives in the repo rather than in anyone's
head.

## Development workflow

`develop` is the default and integration branch. Work happens on short-lived
branches off it and lands via pull request:

1. Branch off `develop`:
   ```
   git checkout develop && git pull
   git checkout -b my-change
   ```
2. Make your change. Keep purely mechanical churn — a `gofmt -w`, a bulk rename —
   in its **own commit**, so the substantive diff reviews cleanly on its own.
3. Build, vet, and test before opening the PR:
   ```
   gofmt -w .
   go build ./...
   go vet ./...
   go test ./...
   ```
4. Open a PR into `develop`.
5. After review, merge into `develop`.

Releases are cut from `develop` (see [Releasing](#releasing)).

## Formatting

`gofmt` output is canonical. Run `gofmt -w .` as part of every change. A
formatting-only diff is a legitimate, self-contained commit, not noise — and
keeping it separate from behavior changes keeps reviews honest.

## Code style

The codebase has a consistent house style; match the surrounding code. In short:

- **Functional options**, not positional config structs (`Option`, `ClaimOpt`,
  `InsertArg`, …).
- **Errors wrapped with an operation prefix**: `fmt.Errorf("operation: %w", err)`.
  Interrogate errors with structured error types and predicate methods
  (`errors.As` + e.g. `HasCollisions()`), not sentinel constants.
- **Small, role-based interfaces** (`Backend`, `Waiter`, `Notifier`), composed
  via fields rather than embedding.
- **Never silently discard an error.** Propagate it, or — for genuinely
  best-effort work (cleanup, optional bookkeeping) — log it with `log.Printf`.
- **Godoc on every exported symbol**, name-led. Doc comments should argue the
  *why* and the trade-offs, not restate the *what*.

## Testing

- Standard-library `testing` only — no third-party assertion or mock libraries.
- Cover both happy and error paths; prefer table-driven tests and `Example_*`
  doctests where they read well.
- The in-memory backend (`pkg/backend/eqmem`) makes most behavior testable
  without any external services — use it for unit tests.

## Changelog

The project follows [Keep a Changelog](https://keepachangelog.com/). Add entries
under a `## [Unreleased]` heading as you go; a release renames that heading to
`## [X.Y.Z] - YYYY-MM-DD`.

## Releasing

Versions are **git-tag-driven**. There is no version constant to edit:
`pkg/version` resolves the version at runtime from, in priority order, an
`-ldflags` injection, the Go toolchain's build info (the module tag), or `"dev"`.

To cut a release from `develop`:

1. In the release PR, finalize the changelog: rename `## [Unreleased]` to
   `## [X.Y.Z] - YYYY-MM-DD`.
2. Merge to `develop`.
3. Tag the merged commit and push the tag:
   ```
   git checkout develop && git pull
   git tag -a vX.Y.Z -m "vX.Y.Z: short summary"
   git push origin vX.Y.Z
   ```

### Versioning stance

We follow [Semantic Versioning](https://semver.org/) in spirit. In practice,
while the user base is small, we do **not** promise a major-version bump for
every breaking change — a breaking change may ship in a minor release, with the
break called out in the changelog. That policy will tighten as adoption grows;
until then, pin a version you depend on and read the changelog before upgrading.

## Commit messages

Conventional-commits style — `feat:`, `fix:`, `chore:`, `docs:`, `style:`,
`refactor:` — with a body that argues the *why*, not just the *what*. Reference
issues and PRs where relevant so the systems stay cross-linked.

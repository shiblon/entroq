# Release Process

## Prerequisites

- `gh` CLI authenticated
- `docker` authenticated to the image registry
- `git flow` installed (optional; see below)
- Working tree clean on `develop`

## Steps

### 1. Prepare the release branch

**Without git-flow:**
```sh
git checkout develop && git pull
```

**With git-flow** (manages the branch; we handle the PR manually):
```sh
git flow release start <version>   # creates release/<version> from develop
```
Do all release prep (steps 2–3 below) on this branch instead of `develop`.

### 2. Update release artifacts

- Add a `## [<version>]` entry to `CHANGELOG.md`.
- Bump `SchemaVersion` in `pkg/backend/eqpg/schema.go` **and** the matching
  `INSERT` in `pkg/backend/eqpg/schema.sql` if the pg schema changed.
  The release script enforces that `SchemaVersion` major.minor matches the
  release tag major.minor.

### 3. Commit and push

```sh
git add -A
git commit -m "chore: prepare release v<version>"
git push origin <branch>   # develop or release/<version>
```

### 4. Open and merge the PR

```sh
gh pr create --base main --title "Release v<version>"
```

Get it reviewed and merged via GitHub. `main` is a protected branch; direct
pushes are not allowed.

**With git-flow:** after the PR merges, also open a PR (or fast-forward merge)
from `release/<version>` → `develop`, then delete the release branch:
```sh
git flow release finish <version>   # skips the main merge (already done via PR);
                                     # merges to develop and tags locally
# OR do it manually:
git checkout develop && git merge main && git push origin develop
git branch -d release/<version>
git push origin --delete release/<version>
```

### 5. Tag the release

After the PR is merged, pull `main` and run the release script:

```sh
git checkout main && git pull
./scripts/tag-release.sh <version>
```

The script verifies:
- Working tree is clean
- No `replace` directives in `go.mod`
- `CHANGELOG.md` has an entry for the version
- `SchemaVersion` major.minor matches the tag major.minor
- Tag does not already exist

Then it creates and pushes `v<version>`.

### 6. Sync develop

```sh
git checkout develop && git merge main && git push origin develop
```

### 7. Build and push Docker images

```sh
./scripts/build-docker.sh <version> --push
```

## Version numbering

- **Patch** (`1.x.y+1`): bug fixes, no schema changes, no new public API.
- **Minor** (`1.x+1.0`): new features, additive schema changes. `SchemaVersion`
  major.minor must match. Existing databases upgrade by re-running
  `eqpg schema upgrade`.
- **Major** (`x+1.0.0`): breaking changes.

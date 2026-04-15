#!/bin/sh
# Create and push a release tag after running pre-flight checks.
# Usage: ./scripts/tag-release.sh <version>
#
# Example: ./scripts/tag-release.sh 1.0.0-rc2
set -e

VERSION="${1}"
if [ -z "${VERSION}" ]; then
    echo "Usage: $0 <version>" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${REPO_ROOT}"

fail() {
    echo "error: $1" >&2
    exit 1
}

# 1. Clean working tree.
if [ -n "$(git status --porcelain)" ]; then
    fail "working tree is not clean; commit or stash changes before tagging"
fi

# 2. No replace directives in go.mod.
if grep -q "^replace" go.mod; then
    echo "error: go.mod contains replace directives:" >&2
    grep "^replace" go.mod >&2
    fail "remove replace directives before tagging a release"
fi

# 3. CHANGELOG.md has an entry for this version.
if ! grep -q "^\#\# \[${VERSION}\]" CHANGELOG.md; then
    fail "no CHANGELOG.md entry found for [${VERSION}]; add one before tagging"
fi

# 4. Tag does not already exist.
if git rev-parse "v${VERSION}" >/dev/null 2>&1; then
    fail "tag v${VERSION} already exists"
fi

echo "Pre-flight checks passed."
echo "Tagging v${VERSION}..."

git tag -a "v${VERSION}" -m "Release ${VERSION}"
git push origin "v${VERSION}"

echo "Tagged and pushed v${VERSION}."
echo "Next: ./scripts/build-docker.sh ${VERSION} --push"

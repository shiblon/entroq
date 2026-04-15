#!/bin/sh
# Publish the JS client to npm.
# Usage: NPM_TOKEN=<token> ./scripts/publish-js.sh <version> [--force]
# Bumps the version in clients/js/package.json, builds, and publishes.
# The version change is left on disk; commit it alongside the release tag.
#
# Pre-release versions (rc, alpha, beta, dev) are blocked without --force.
# RC builds should be tested via git install, not npm:
#   npm install github:shiblon/entroq#<tag>
set -e

VERSION="${1}"
FORCE="${2}"

if [ -z "${VERSION}" ]; then
    echo "Usage: $0 <version> [--force]" >&2
    exit 1
fi

if [ -z "${NPM_TOKEN}" ]; then
    echo "NPM_TOKEN is not set" >&2
    exit 1
fi

# Block pre-release versions unless --force is passed.
case "${VERSION}" in
    *rc* | *alpha* | *beta* | *dev*)
        if [ "${FORCE}" != "--force" ]; then
            echo "error: '${VERSION}' looks like a pre-release." >&2
            echo "npm versions are permanent. Test pre-releases via git install instead:" >&2
            echo "  npm install github:shiblon/entroq#${VERSION}" >&2
            echo "Re-run with --force to publish anyway." >&2
            exit 1
        fi
        echo "warning: publishing pre-release version ${VERSION} to npm (--force)" >&2
        ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PKGJSON="${REPO_ROOT}/clients/js/package.json"

# Bump version in package.json.
sed -i.bak "s/\"version\": \"[^\"]*\"/\"version\": \"${VERSION}\"/" "${PKGJSON}"
rm -f "${PKGJSON}.bak"
echo "Set version ${VERSION} in ${PKGJSON}"

docker run --rm \
    -v "${REPO_ROOT}/clients/js:/work" \
    -w /work \
    -e NPM_TOKEN="${NPM_TOKEN}" \
    node:20-alpine \
    sh -c "npm install && npm run build && echo '//registry.npmjs.org/:_authToken=${NPM_TOKEN}' > .npmrc && npm publish && rm .npmrc"

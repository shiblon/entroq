#!/bin/sh
# Publish the JS client to npm.
# Builds first, then publishes. Requires NPM_TOKEN in the environment.
set -e

if [ -z "${NPM_TOKEN}" ]; then
    echo "NPM_TOKEN is not set" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

docker run --rm \
    -v "${REPO_ROOT}/clients/js:/work" \
    -w /work \
    -e NPM_TOKEN="${NPM_TOKEN}" \
    node:20-alpine \
    sh -c "npm install && npm run build && echo '//registry.npmjs.org/:_authToken=${NPM_TOKEN}' > .npmrc && npm publish && rm .npmrc"

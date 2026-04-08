#!/bin/sh
# Build the JS client using a pinned Node image.
# Outputs CJS, ESM, and type declarations into clients/js/dist/.
set -e

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

docker run --rm \
    -v "${REPO_ROOT}/clients/js:/work" \
    -w /work \
    node:20-alpine \
    sh -c "npm install && npm run build"

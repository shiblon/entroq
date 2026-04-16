#!/bin/sh
# Build and optionally push EntroQ Docker images to ghcr.io.
# Usage: ./scripts/build-docker.sh <version> [--push]
#
# Builds:
#   ghcr.io/shiblon/entroq-pg:<version>     -- PostgreSQL-backed gRPC service
#   ghcr.io/shiblon/entroq-mem:<version>    -- in-memory gRPC service (with journal)
#   ghcr.io/shiblon/entroq-redis:<version>  -- Redis-backed gRPC service
#
# Pass --push to push to ghcr.io after building.
# Before pushing for the first time, authenticate with:
#   echo <github-pat> | docker login ghcr.io -u shiblon --password-stdin
# The PAT needs the write:packages scope.
set -e

VERSION="${1}"
if [ -z "${VERSION}" ]; then
    echo "Usage: $0 <version> [--push]" >&2
    exit 1
fi

PUSH=0
if [ "${2}" = "--push" ]; then
    PUSH=1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REGISTRY="ghcr.io/shiblon"

build_image() {
    local name="${1}"
    local dockerfile="${2}"
    echo "--- Building ${name} ---"
    docker build \
        --build-arg "VERSION=${VERSION}" \
        -f "${REPO_ROOT}/${dockerfile}" \
        -t "${REGISTRY}/${name}:${VERSION}" \
        -t "${REGISTRY}/${name}:latest" \
        "${REPO_ROOT}"
    echo "Built ${REGISTRY}/${name}:${VERSION}"
}

build_image entroq-pg    cmd/eqpg/Dockerfile
build_image entroq-mem   cmd/eqmem/Dockerfile
build_image entroq-redis cmd/eqredis/Dockerfile

if [ "${PUSH}" = "1" ]; then
    echo "--- Pushing images ---"
    docker push "${REGISTRY}/entroq-pg:${VERSION}"
    docker push "${REGISTRY}/entroq-pg:latest"
    docker push "${REGISTRY}/entroq-mem:${VERSION}"
    docker push "${REGISTRY}/entroq-mem:latest"
    docker push "${REGISTRY}/entroq-redis:${VERSION}"
    docker push "${REGISTRY}/entroq-redis:latest"
    echo "Pushed entroq-pg, entroq-mem, entroq-redis at ${VERSION}"
fi

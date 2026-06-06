#!/bin/sh
# Build and optionally push EntroQ Docker images to ghcr.io.
# Usage: ./scripts/build-docker.sh <version> [--push]
#
# Builds (tags: <version>, <major>.<minor> for stable releases, latest):
#   ghcr.io/shiblon/entroq-pg:<version>       -- PostgreSQL-backed gRPC service
#   ghcr.io/shiblon/entroq-mem:<version>      -- in-memory gRPC service (with journal)
#   ghcr.io/shiblon/entroq-redis:<version>    -- Redis-backed gRPC service
#   ghcr.io/shiblon/entroq-operator:<version> -- Kubernetes mesh operator
#   ghcr.io/shiblon/entroq-link:<version>     -- eqlink sidecar / async mesh proxy
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
case "${VERSION}" in
    v*) echo "error: version must not have a 'v' prefix (got '${VERSION}'); use '${VERSION#v}'" >&2; exit 1 ;;
esac

PUSH=0
if [ "${2}" = "--push" ]; then
    PUSH=1
fi

# Derive major.minor alias for stable releases only (X.Y.Z with no pre-release suffix).
MINOR_TAG=""
if echo "${VERSION}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    MINOR_TAG="$(echo "${VERSION}" | sed 's/^\([0-9]*\.[0-9]*\).*/\1/')"
fi

cd "$(dirname "$0")/.."
echo "Repo root: $PWD"

REGISTRY="ghcr.io/shiblon"

build_image() {
  local name="${1}"
  local dockerfile="${2}"
  echo "--- Building ${name} ---"
  docker build \
    --build-arg "VERSION=${VERSION}" \
    -f "./${dockerfile}" \
    -t "${REGISTRY}/${name}:${VERSION}" \
    -t "${REGISTRY}/${name}:latest" \
    .
  if [ -n "${MINOR_TAG}" ]; then
    docker tag "${REGISTRY}/${name}:${VERSION}" "${REGISTRY}/${name}:${MINOR_TAG}"
  fi
  echo "Built ${REGISTRY}/${name}:${VERSION}"
}

build_image entroq-pg cmd/eqpg/Dockerfile
build_image entroq-mem cmd/eqmem/Dockerfile
build_image entroq-redis cmd/eqredis/Dockerfile
build_image entroq-operator cmd/eqk8s/Dockerfile
build_image entroq-link cmd/eqlink/Dockerfile

if [ "${PUSH}" = "1" ]; then
  echo "--- Pushing images ---"
  docker push "${REGISTRY}/entroq-pg:${VERSION}"
  docker push "${REGISTRY}/entroq-pg:latest"
  docker push "${REGISTRY}/entroq-mem:${VERSION}"
  docker push "${REGISTRY}/entroq-mem:latest"
  docker push "${REGISTRY}/entroq-redis:${VERSION}"
  docker push "${REGISTRY}/entroq-redis:latest"
  docker push "${REGISTRY}/entroq-operator:${VERSION}"
  docker push "${REGISTRY}/entroq-operator:latest"
  docker push "${REGISTRY}/entroq-link:${VERSION}"
  docker push "${REGISTRY}/entroq-link:latest"
  if [ -n "${MINOR_TAG}" ]; then
    docker push "${REGISTRY}/entroq-pg:${MINOR_TAG}"
    docker push "${REGISTRY}/entroq-mem:${MINOR_TAG}"
    docker push "${REGISTRY}/entroq-redis:${MINOR_TAG}"
    docker push "${REGISTRY}/entroq-operator:${MINOR_TAG}"
    docker push "${REGISTRY}/entroq-link:${MINOR_TAG}"
  fi
  echo "Pushed entroq-pg, entroq-mem, entroq-redis, entroq-operator, entroq-link at ${VERSION}"
fi

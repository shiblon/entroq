#!/bin/sh
# Publish the Python client to PyPI.
# Usage: PYPI_TOKEN=<token> ./scripts/publish-py.sh <version> [--force]
# Bumps the version in clients/py/pyproject.toml, builds, and uploads via twine.
# The version change is left on disk; commit it alongside the release tag.
#
# Pre-release versions (rc, alpha, beta, dev) are blocked without --force.
# RC builds should be tested via git install, not PyPI:
#   pip install git+https://github.com/shiblon/entroq.git@<tag>
set -e

VERSION="${1}"
FORCE="${2}"

if [ -z "${VERSION}" ]; then
    echo "Usage: $0 <version> [--force]" >&2
    exit 1
fi

if [ -z "${PYPI_TOKEN}" ]; then
    echo "PYPI_TOKEN is not set" >&2
    exit 1
fi

# Block pre-release versions unless --force is passed.
case "${VERSION}" in
    *rc* | *alpha* | *beta* | *dev*)
        if [ "${FORCE}" != "--force" ]; then
            echo "error: '${VERSION}' looks like a pre-release." >&2
            echo "PyPI versions are permanent. Test pre-releases via git install instead:" >&2
            echo "  pip install git+https://github.com/shiblon/entroq.git@${VERSION}" >&2
            echo "Re-run with --force to publish anyway." >&2
            exit 1
        fi
        echo "warning: publishing pre-release version ${VERSION} to PyPI (--force)" >&2
        ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PYPROJECT="${REPO_ROOT}/clients/py/pyproject.toml"

# Bump version in pyproject.toml.
sed -i.bak "s/^version = .*/version = \"${VERSION}\"/" "${PYPROJECT}"
rm -f "${PYPROJECT}.bak"
echo "Set version ${VERSION} in ${PYPROJECT}"

docker run --rm \
    -v "${REPO_ROOT}/clients/py:/work" \
    -w /work \
    -e PYPI_TOKEN="${PYPI_TOKEN}" \
    python:3.12-slim \
    sh -c "pip install --quiet build twine && python -m build && twine upload --non-interactive -u __token__ -p \"${PYPI_TOKEN}\" dist/*"

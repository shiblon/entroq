#!/bin/sh
# Publish the Python client to PyPI.
# Builds a source distribution and wheel, then uploads via twine.
# Requires PYPI_TOKEN in the environment.
set -e

if [ -z "${PYPI_TOKEN}" ]; then
    echo "PYPI_TOKEN is not set" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

docker run --rm \
    -v "${REPO_ROOT}/clients/py:/work" \
    -w /work \
    -e PYPI_TOKEN="${PYPI_TOKEN}" \
    python:3.12-slim \
    sh -c "pip install --quiet build twine && python -m build && twine upload --non-interactive -u __token__ -p \"${PYPI_TOKEN}\" dist/*"

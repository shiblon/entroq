#!/bin/bash
# Build the entroq Docker image and apply version tags.
#
# Usage: docker_build.sh <version>
#   e.g. docker_build.sh 1.2.3
#
# Builds the image and tags it as:
#   shiblon/entroq:1.2.3
#   shiblon/entroq:1.2
#   shiblon/entroq:1

# If you want to build latest, you have to retag manually, as that's a dangerous
# default to apply to all builds. You can do it like this:
#   docker tag shiblon/entroq:1.2.3 shiblon/entroq:latest
#
# Does not push. Run 'docker push shiblon/entroq --all-tags' when ready.

set -euo pipefail

REPO="shiblon/entroq"

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <version>" >&2
    echo "  e.g. $0 1.2.3" >&2
    exit 1
fi

VERSION="$1"

# Strip a leading 'v' if provided, since Docker tags don't use it.
VERSION="${VERSION#v}"

if [[ ! "$VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    echo "Error: version must be semver like 1.2.3" >&2
    exit 1
fi

MAJOR_MINOR="${VERSION%.*}"   # 1.2.3 -> 1.2
MAJOR="${VERSION%%.*}"        # 1.2.3 -> 1

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"

echo "Building $REPO:$VERSION from $REPO_ROOT ..."
docker build \
    -t "$REPO:$VERSION" \
    -t "$REPO:$MAJOR_MINOR" \
    -t "$REPO:$MAJOR" \
    "$REPO_ROOT"

echo ""
echo "Built tags:"
echo "  $REPO:$VERSION"
echo "  $REPO:$MAJOR_MINOR"
echo "  $REPO:$MAJOR"
echo ""
echo "To push: docker push $REPO --all-tags"

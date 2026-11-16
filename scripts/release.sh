#!/bin/sh
# Prepares a release locally: verifies the checkout, runs every gate, builds the
# archives and the image, and runs the acceptance suite. It never tags, pushes
# or publishes; those steps are printed at the end for the maintainer to run.
#
#   scripts/release.sh 1.0.0
set -eu
cd "$(dirname "$0")/.."

version=${1:?usage: scripts/release.sh <version without v>}
tag=v$version

if [ -n "$(git status --porcelain)" ]; then
    echo "the working tree is not clean" >&2
    exit 1
fi
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
    if [ "$(git rev-parse "$tag^{commit}")" != "$(git rev-parse HEAD)" ]; then
        echo "tag $tag does not point at HEAD" >&2
        exit 1
    fi
fi
if ! grep -q "^## \[\?$version\]\?" CHANGELOG.md; then
    echo "CHANGELOG.md has no entry for $version" >&2
    exit 1
fi

./scripts/ci.sh
make release VERSION="$version"
make docker VERSION="$version" IMAGE="${IMAGE:-larder}"

# The acceptance suite needs docker, node and the public registries.
if [ "${SKIP_ACCEPTANCE:-0}" != "1" ]; then
    ./scripts/acceptance.sh
else
    echo "acceptance skipped on request: the release is NOT validated against real clients" >&2
fi

cat <<NEXT

Release $version is built:
  dist/                       archives and SHA256SUMS
  ${IMAGE:-larder}:$version   local image

To publish, from this commit:
  git tag -s $tag -m "Larder $version"
  git push origin $tag
The release workflow then rebuilds, runs the acceptance suite and publishes
the archives and the image.
NEXT

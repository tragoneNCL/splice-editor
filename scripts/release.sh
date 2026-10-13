#!/usr/bin/env bash
# Ship an update: tests, tag, push. GitHub Actions then builds the downloads for
# every platform and publishes the release; running apps pick it up on their
# next check (within ~15 minutes) or on next launch.
#
#   scripts/release.sh v1.0.1
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:?usage: scripts/release.sh vX.Y.Z}"
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must look like v1.2.3"; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "commit your changes first"; exit 1; }
go vet ./... && go test ./...
git tag "$VERSION"
git push origin main "$VERSION"
echo "Tagged $VERSION. Follow the build: gh run watch"

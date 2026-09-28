#!/usr/bin/env bash
# Build the cpa-cursor plugin as a versioned shared library in dist/.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="$(sed -n 's/.*pluginVersion *= *"\(.*\)".*/\1/p' main.go | head -n1)"
[[ -n "${VERSION}" ]] || { echo "could not determine the plugin version"; exit 1; }

mkdir -p dist
go build -trimpath -buildmode=c-shared -o "dist/cpa-cursor-v${VERSION}.so" .
sha256sum "dist/cpa-cursor-v${VERSION}.so" > "dist/cpa-cursor-v${VERSION}.so.sha256"
echo "built dist/cpa-cursor-v${VERSION}.so"

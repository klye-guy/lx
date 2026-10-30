#!/usr/bin/env bash
# Build static lx binaries for every common Linux architecture.
# Each lands in dist/<arch>/lx, ready to copy straight onto your PATH.
set -euo pipefail
cd "$(dirname "$0")"
rm -rf dist
for arch in amd64 arm64 386 arm riscv64 ppc64le s390x; do
    mkdir -p "dist/$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -ldflags="-s -w" -o "dist/$arch/lx" .
    echo "built dist/$arch/lx"
done

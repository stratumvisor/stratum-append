#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
mkdir -p dist

for arch in amd64 arm64; do
    echo "Building STRATUM Container Appender for linux/${arch} (static)..."
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -buildvcs=false -ldflags='-s -w' \
        -o "dist/stratum-container-append-linux-${arch}" \
        ./cmd/stratum-container-append
done

(
    cd dist
    sha256sum stratum-container-append-linux-* > SHA256SUMS
)

echo "Binaries and checksums written to dist/"

#!/usr/bin/env bash
set -euo pipefail
# Keep the collector separate from the shipped module's dependency graph.
notice_tools=$(mktemp -d)
trap 'rm -rf "$notice_tools"' EXIT
GOBIN="$notice_tools" go install github.com/google/go-licenses/v2@v2.0.1
rm -rf THIRD_PARTY_NOTICES
for arch in amd64 arm64; do
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 "$notice_tools/go-licenses" save \
    ./cmd/gpu-mode ./cmd/gpu-workload-proxy \
    --ignore github.com/mickey-kras/gpu-workload-supervisor \
    --save_path="THIRD_PARTY_NOTICES/linux_$arch"
done
# go-licenses excludes the standard library; shipped Go runtime code needs its notice.
mkdir -p THIRD_PARTY_NOTICES/go
cp "$(go env GOROOT)/LICENSE" THIRD_PARTY_NOTICES/go/LICENSE

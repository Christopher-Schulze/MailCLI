#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
check_go_toolchain "${MAILCLI_ROOT}"
MAILCLI_BINARY_OUTPUT="${MAILCLI_BUILD_OUTPUT:-${MAILCLI_ROOT}/bin/mailcli}"

mkdir -p "$(dirname "${MAILCLI_BINARY_OUTPUT}")"
# go build leaves an up-to-date output untouched, which would keep a binary
# that install-local.sh re-signed; always link a fresh one.
rm -f "${MAILCLI_BINARY_OUTPUT}"
# Retain compiler inlining for hot paths; stripping and native dead-code removal
# keep the executable small.
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go -C "${MAILCLI_ROOT}" build \
  -buildvcs=false \
  -ldflags='-s -w -extldflags=-dead_strip' \
  -mod=readonly \
  -trimpath \
  -o "${MAILCLI_BINARY_OUTPUT}" \
  ./cmd/mailcli

# Keep the Go-derived UUID: macOS 26 requires LC_UUID to launch the binary.
strip "${MAILCLI_BINARY_OUTPUT}"
file "${MAILCLI_BINARY_OUTPUT}"

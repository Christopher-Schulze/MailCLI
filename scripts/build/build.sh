#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
check_go_toolchain "${MAILCLI_ROOT}"
MAILCLI_OUTPUT="${MAILCLI_BUILD_OUTPUT:-${MAILCLI_ROOT}/bin/mailcli}"

mkdir -p "$(dirname "${MAILCLI_OUTPUT}")"
# Retain compiler inlining for hot paths; stripping and native dead-code removal
# keep the executable within the size budget enforced by the release gate.
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go -C "${MAILCLI_ROOT}" build \
  -buildvcs=false \
  -ldflags='-s -w -extldflags=-dead_strip' \
  -mod=readonly \
  -trimpath \
  -o "${MAILCLI_OUTPUT}" \
  ./cmd/mailcli

strip -no_uuid "${MAILCLI_OUTPUT}"
file "${MAILCLI_OUTPUT}"

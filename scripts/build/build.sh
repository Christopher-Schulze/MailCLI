#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_OUTPUT="${MAILCLI_ROOT}/bin/mailcli"

mkdir -p "${MAILCLI_ROOT}/bin"
# Keep the typed operation-guidance additions below the enforced release size
# budget while leaving debug and test builds on the default compiler settings.
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -buildvcs=false \
  -gcflags='mailcli/internal/mail=-l' \
  -ldflags='-s -w -extldflags=-dead_strip' \
  -mod=readonly \
  -trimpath \
  -o "${MAILCLI_OUTPUT}" \
  "${MAILCLI_ROOT}/cmd/mailcli"

file "${MAILCLI_OUTPUT}"

#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_OUTPUT="${MAILCLI_ROOT}/bin/mailcli"

mkdir -p "${MAILCLI_ROOT}/bin"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -trimpath \
  -o "${MAILCLI_OUTPUT}" \
  "${MAILCLI_ROOT}/cmd/mailcli"

file "${MAILCLI_OUTPUT}"

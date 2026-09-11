#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_OUTPUT="${MAILCLI_ROOT}/bin/mailcli"

mkdir -p "${MAILCLI_ROOT}/bin"
# Keep mail operations, store access, CLI dispatch, and IMAP parsing within budget;
# debug and test builds retain the default compiler settings.
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -buildvcs=false \
  -gcflags='mailcli/internal/mail=-l' \
  -gcflags='mailcli/internal/cli=-l' \
  -gcflags='mailcli/internal/transport/imapclient=-l' \
  -gcflags='mailcli/internal/mailstore=-l' \
  -ldflags='-s -w -extldflags=-dead_strip' \
  -mod=readonly \
  -trimpath \
  -o "${MAILCLI_OUTPUT}" \
  "${MAILCLI_ROOT}/cmd/mailcli"

strip -no_uuid "${MAILCLI_OUTPUT}"
file "${MAILCLI_OUTPUT}"

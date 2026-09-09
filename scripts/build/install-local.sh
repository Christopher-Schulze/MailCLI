#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_DESTINATION="${1:-${HOME}/.local/bin/mailcli}"

if [[ "$#" -gt 1 ]]; then
  printf 'Usage: %s [BINARY_DESTINATION]\n' "$(basename "${BASH_SOURCE[0]}")" >&2
  exit 2
fi
if [[ ! -x "${MAILCLI_ROOT}/scripts/release/install.sh" ]]; then
  printf 'Shared installer is missing or not executable: %s\n' "${MAILCLI_ROOT}/scripts/release/install.sh" >&2
  exit 1
fi

"${MAILCLI_ROOT}/scripts/build/build.sh"
MAILCLI_INSTALL_PACKAGE_ROOT="${MAILCLI_ROOT}" \
MAILCLI_BINARY_DESTINATION="${MAILCLI_DESTINATION}" \
  "${MAILCLI_ROOT}/scripts/release/install.sh"

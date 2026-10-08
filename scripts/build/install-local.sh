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

# Test builds may redirect the compiled binary away from bin/mailcli. The
# shared installer still consumes a package-shaped source, so stage the
# redirected output privately instead of touching the production binary.
INSTALL_PACKAGE_ROOT="${MAILCLI_ROOT}"
if [[ -n "${MAILCLI_BUILD_OUTPUT:-}" &&
  "${MAILCLI_BUILD_OUTPUT}" != "${MAILCLI_ROOT}/bin/mailcli" ]]; then
  STAGING_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-install-package.XXXXXX")"
  trap 'rm -rf -- "${STAGING_ROOT}"' EXIT
  mkdir -p "${STAGING_ROOT}/bin" "${STAGING_ROOT}/skills"
  cp "${MAILCLI_BUILD_OUTPUT}" "${STAGING_ROOT}/bin/mailcli"
  cp -R "${MAILCLI_ROOT}/skills/mailcli" "${STAGING_ROOT}/skills/mailcli"
  chmod 0755 "${STAGING_ROOT}/bin/mailcli"
  INSTALL_PACKAGE_ROOT="${STAGING_ROOT}"
fi

MAILCLI_INSTALL_PACKAGE_ROOT="${INSTALL_PACKAGE_ROOT}" \
MAILCLI_BINARY_DESTINATION="${MAILCLI_DESTINATION}" \
  "${MAILCLI_ROOT}/scripts/release/install.sh"

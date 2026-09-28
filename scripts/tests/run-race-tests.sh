#!/usr/bin/env bash
set -euo pipefail

# Manual race-detector run. No gate, CI workflow or full proof invokes it;
# start it explicitly when a race check is wanted, e.g. before a release.
MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
check_go_toolchain "${MAILCLI_ROOT}"

if [[ "$#" -gt 0 ]]; then
  PACKAGES=("$@")
else
  PACKAGES=(./...)
fi
cd "${MAILCLI_ROOT}"
MAILCLI_LIVE_TESTS= MAILCLI_KEYCHAIN_LIVE= go test -count=1 -race "${PACKAGES[@]}"
printf 'race_tests=passed\n'

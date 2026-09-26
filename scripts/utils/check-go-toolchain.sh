#!/usr/bin/env bash

check_go_toolchain() {
  local ROOT="$1"
  local PIN
  local ACTUAL
  PIN="$(awk '$1 == "go" { print $2 }' "${ROOT}/go.mod")" || return 2
  [[ "${PIN}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    printf 'go.mod must pin one exact stable Go version\n' >&2
    return 2
  }
  if [[ -n "${GOTOOLCHAIN:-}" && "${GOTOOLCHAIN}" != "go${PIN}" ]]; then
    printf 'Conflicting GOTOOLCHAIN: expected go%s, received %s\n' "${PIN}" "${GOTOOLCHAIN}" >&2
    return 2
  fi
  export GOTOOLCHAIN="go${PIN}"
  ACTUAL="$(cd "${ROOT}" && go env GOVERSION)" || return 2
  [[ "${ACTUAL}" == "${GOTOOLCHAIN}" ]] || {
    printf 'Actual Go toolchain differs from pin: expected %s, received %s\n' "${GOTOOLCHAIN}" "${ACTUAL}" >&2
    return 2
  }
  printf 'go_toolchain=%s\n' "${ACTUAL}"
}

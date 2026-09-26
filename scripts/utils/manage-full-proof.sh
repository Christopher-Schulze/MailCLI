#!/usr/bin/env bash
set -euo pipefail

validate_full_receipt() {
  local PROOF_PATH="$1" LOG_PATH="$2" IDENTITY="$3" EXPECTED_HEAD="$4" EXPECTED_TREE="$5"
  [[ -f "${PROOF_PATH}" && ! -L "${PROOF_PATH}" && -f "${LOG_PATH}" && ! -L "${LOG_PATH}" ]] || return 1
  grep -qx 'gate_tier=full' "${LOG_PATH}" || return 1
  grep -qx 'vulnerability_check=passed' "${LOG_PATH}" || return 1
  grep -Eq $'^shell_test_receipt=[0-9a-f]{64}\tcore-checks$' "${LOG_PATH}" || return 1
  local RECEIPT_HASH
  RECEIPT_HASH="$(shasum -a 256 "${LOG_PATH}" | awk '{print $1}')" || return 1
  jq -e --arg identity "${IDENTITY}" --arg head "${EXPECTED_HEAD}" --arg tree "${EXPECTED_TREE}" --arg receipt "${RECEIPT_HASH}" \
    '.schema == 1 and .identity == $identity and .head == $head and .tree == $tree and .full_gate == "passed" and .vulnerability_check == "passed" and .receipt_sha256 == $receipt' \
    "${PROOF_PATH}" >/dev/null
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0

[[ "$#" == 2 && ( "$1" == run || "$1" == check ) ]] || {
  printf 'Usage: manage-full-proof.sh run|check ROOT\n' >&2
  exit 2
}
MODE="$1"
for FLAG in MAILCLI_LIVE_TESTS MAILCLI_KEYCHAIN_LIVE MAILCLI_LIVE_RESPONSIVENESS; do
  [[ "${!FLAG:-}" != 1 ]] || { printf 'Full push proof is non-live; use --full-checks for explicitly requested live stages\n' >&2; exit 2; }
done
ROOT="$(cd "$2" && pwd -P)"
cd "${ROOT}"
export GOWORK=off
GIT_DIRECTORY="$(git rev-parse --absolute-git-dir)"
COMMON_DIRECTORY="$(git rev-parse --path-format=absolute --git-common-dir)"
PROOF="${GIT_DIRECTORY}/mailcli-full-proof.json"
PROOF_LOG="${GIT_DIRECTORY}/mailcli-full-proof.log"

full_identity() {
  [[ ! -e "${COMMON_DIRECTORY}/mailcli-write-reservations.lock" &&
    ! -L "${COMMON_DIRECTORY}/mailcli-write-reservations" &&
    -z "$(find "${COMMON_DIRECTORY}/mailcli-write-reservations" -mindepth 1 -maxdepth 1 -print 2>/dev/null)" ]] || {
    printf 'Full proof requires no shared worker reservations or registry mutation\n' >&2
    return 1
  }
  [[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || {
    printf 'Full proof requires a clean HEAD, index and worktree\n' >&2
    return 1
  }
  [[ ! -d "${GIT_DIRECTORY}/mailcli-write-lease" ]] || {
    printf 'Full proof requires the integrated queue with no active writer\n' >&2
    return 1
  }
  [[ -z "$(git ls-files -v | grep -E '^[a-zS] ' || true)" ]] || {
    printf 'Full proof refuses hidden tracked index flags\n' >&2
    return 1
  }
  source "${ROOT}/scripts/utils/check-go-toolchain.sh"
  check_go_toolchain "${ROOT}" >&2 || return "$?"
  {
    git rev-parse HEAD HEAD^{tree} || return 1
    git write-tree || return 1
    go env -json GOVERSION GOOS GOARCH CGO_ENABLED GOFLAGS GOWORK CC CXX CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS || return 1
    for TOOL in golangci-lint govulncheck; do
      TOOL_PATH="$(command -v "${TOOL}" || true)"
      [[ -n "${TOOL_PATH}" ]] || TOOL_PATH="$(go env GOPATH)/bin/${TOOL}"
      [[ -x "${TOOL_PATH}" ]] || { printf 'Full proof requires %s\n' "${TOOL}" >&2; return 1; }
      shasum -a 256 "${TOOL_PATH}" | awk '{print $1}' || return 1
    done
    printf 'cpus=%s\npackages=%s\n' "${MAILCLI_TEST_CPUS:-4}" "${MAILCLI_TEST_PACKAGES:-2}"
  } | shasum -a 256 | awk '{print $1}'
}

IDENTITY="$(full_identity)"
if validate_full_receipt "${PROOF}" "${PROOF_LOG}" "${IDENTITY}" "$(git rev-parse HEAD)" "$(git write-tree)"; then
  printf 'full_proof=valid\nfull_proof_reused=true\nhead=%s\n' "$(git rev-parse HEAD)"
  exit 0
fi
[[ "${MODE}" == run ]] || { printf 'Full proof is missing, incomplete or stale\n' >&2; exit 1; }
[[ ! -L "${PROOF}" ]] || { printf 'Full proof path must not be a symlink\n' >&2; exit 1; }
[[ ! -L "${PROOF_LOG}" ]] || { printf 'Full proof log must not be a symlink\n' >&2; exit 1; }
umask 077
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-full-proof.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
HEAD="$(git rev-parse HEAD)"
TREE="$(git write-tree)"
"${ROOT}/scripts/utils/run-staged-gate.sh" "${ROOT}" "${HEAD}" "${TREE}" '' --full | tee "${TEST_ROOT}/output"
grep -qx 'gate_tier=full' "${TEST_ROOT}/output"
grep -qx 'vulnerability_check=passed' "${TEST_ROOT}/output"
[[ "$(full_identity)" == "${IDENTITY}" ]] || { printf 'Full verification source/environment changed\n' >&2; exit 1; }
RECEIPT_HASH="$(shasum -a 256 "${TEST_ROOT}/output" | awk '{print $1}')"
jq -n --arg identity "${IDENTITY}" --arg head "${HEAD}" --arg tree "${TREE}" --arg receipt "${RECEIPT_HASH}" \
  '{schema:1,identity:$identity,head:$head,tree:$tree,receipt_sha256:$receipt,full_gate:"passed",vulnerability_check:"passed"}' >"${TEST_ROOT}/proof"
validate_full_receipt "${TEST_ROOT}/proof" "${TEST_ROOT}/output" "${IDENTITY}" "${HEAD}" "${TREE}"
[[ ! -e "${PROOF}" ]] || {
  [[ -f "${PROOF}" && ! -L "${PROOF}" ]] || exit 1
  mv "${PROOF}" "${TEST_ROOT}/previous-proof"
}
[[ ! -e "${PROOF_LOG}" ]] || {
  [[ -f "${PROOF_LOG}" && ! -L "${PROOF_LOG}" ]] || exit 1
  mv "${PROOF_LOG}" "${TEST_ROOT}/previous-log"
}
mv "${TEST_ROOT}/output" "${PROOF_LOG}"
mv "${TEST_ROOT}/proof" "${PROOF}"
printf 'full_proof=recorded\nfull_gate=passed\nhead=%s\ntree=%s\n' "${HEAD}" "${TREE}"

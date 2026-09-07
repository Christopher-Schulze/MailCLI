#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_ROOT="${MAILCLI_WRITE_ROOT:-${SCRIPT_ROOT}}"

usage() {
  printf '%s\n' \
    'Usage:' \
    '  manage-write-lease.sh acquire TASK_ID OWNER PATH [PATH...]' \
    '  manage-write-lease.sh status' \
    '  manage-write-lease.sh review TOKEN' \
    '  manage-write-lease.sh gate TOKEN' \
    '  manage-write-lease.sh release TOKEN' \
    '  manage-write-lease.sh abort TOKEN'
}

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

[[ -d "${MAILCLI_ROOT}" ]] || fail "Write root does not exist: ${MAILCLI_ROOT}"
MAILCLI_ROOT="$(cd "${MAILCLI_ROOT}" && pwd -P)"
GIT_ROOT="$(git -C "${MAILCLI_ROOT}" rev-parse --show-toplevel 2>/dev/null)" ||
  fail "Write root is not a Git worktree: ${MAILCLI_ROOT}"
GIT_ROOT="$(cd "${GIT_ROOT}" && pwd -P)"
[[ "${GIT_ROOT}" == "${MAILCLI_ROOT}" ]] ||
  fail "Write root must be the worktree root: ${GIT_ROOT}"
GIT_DIRECTORY="$(git -C "${MAILCLI_ROOT}" rev-parse --absolute-git-dir)"
LEASE_DIRECTORY="${GIT_DIRECTORY}/mailcli-write-lease"

lease_file() {
  printf '%s/%s\n' "${LEASE_DIRECTORY}" "$1"
}

remove_lease_files() {
  local NAME
  for NAME in task owner pid token acquired_at baseline_head baseline_status \
    allowed_paths allowed_fingerprints reviewed_digest reviewed_patch_sha256 \
    reviewed_at gate_patch_sha256 gate_head gate_at changed_paths; do
    rm -f "${LEASE_DIRECTORY}/${NAME}"
  done
  rmdir "${LEASE_DIRECTORY}" 2>/dev/null || true
}

require_lease() {
  [[ -d "${LEASE_DIRECTORY}" ]] || fail "No write lease is active"
  [[ -f "$(lease_file token)" ]] || fail "Write lease metadata is incomplete"
}

require_token() {
  local PROVIDED_TOKEN="$1"
  local EXPECTED_TOKEN
  require_lease
  EXPECTED_TOKEN="$(<"$(lease_file token)")"
  [[ -n "${PROVIDED_TOKEN}" && "${PROVIDED_TOKEN}" == "${EXPECTED_TOKEN}" ]] ||
    fail "Write lease token does not match the active owner"
}

fingerprint_path() {
  local RELATIVE_PATH="$1"
  local ABSOLUTE_PATH="${MAILCLI_ROOT}/${RELATIVE_PATH}"
  if [[ -L "${ABSOLUTE_PATH}" ]]; then
    printf 'symlink:%s' "$(readlink "${ABSOLUTE_PATH}")"
  elif [[ -f "${ABSOLUTE_PATH}" ]]; then
    printf 'file:%s' "$(shasum -a 256 "${ABSOLUTE_PATH}" | awk '{print $1}')"
  elif [[ -e "${ABSOLUTE_PATH}" ]]; then
    printf 'unsupported'
  else
    printf 'absent'
  fi
}

path_is_allowed() {
  grep -Fxq -- "$1" "$(lease_file allowed_paths)"
}

allowed_state_digest() {
  local RELATIVE_PATH
  {
    printf 'head=%s\n' "$(<"$(lease_file baseline_head)")"
    while IFS= read -r RELATIVE_PATH; do
      printf '%s\t%s\n' "${RELATIVE_PATH}" "$(fingerprint_path "${RELATIVE_PATH}")"
    done <"$(lease_file allowed_paths)"
  } | shasum -a 256 | awk '{print $1}'
}

staged_patch_digest() {
  local BASELINE_HEAD
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  git -C "${MAILCLI_ROOT}" diff --cached --binary "${BASELINE_HEAD}" -- |
    shasum -a 256 | awk '{print $1}'
}

commit_patch_digest() {
  local BASELINE_HEAD="$1"
  local CURRENT_HEAD="$2"
  git -C "${MAILCLI_ROOT}" diff --binary "${BASELINE_HEAD}" "${CURRENT_HEAD}" -- |
    shasum -a 256 | awk '{print $1}'
}

validate_allowed_path() {
  local RELATIVE_PATH="$1"
  [[ -n "${RELATIVE_PATH}" ]] || fail "Allowed path must not be empty"
  case "${RELATIVE_PATH}" in
    /* | . | ./* | */. | */./* | *//* | .. | ../* | */.. | */../*)
      fail "Allowed path must stay inside the worktree: ${RELATIVE_PATH}"
      ;;
  esac
  [[ "${RELATIVE_PATH}" != *$'\n'* && "${RELATIVE_PATH}" != *$'\t'* ]] ||
    fail "Allowed path must not contain tabs or newlines"
  [[ ! -d "${MAILCLI_ROOT}/${RELATIVE_PATH}" ]] ||
    fail "Allowed path must identify a file, not a directory: ${RELATIVE_PATH}"
}

acquire_lease() {
  [[ "$#" -ge 3 ]] || {
    usage >&2
    exit 2
  }
  local TASK_ID="$1"
  local OWNER="$2"
  shift 2
  [[ "${TASK_ID}" =~ ^[0-9]{3}$ ]] || fail "TASK_ID must be exactly three digits"
  [[ -n "${OWNER}" && "${OWNER}" != *$'\n'* ]] || fail "OWNER must be one line"
  local RELATIVE_PATH
  for RELATIVE_PATH in "$@"; do
    validate_allowed_path "${RELATIVE_PATH}"
  done

  if ! mkdir "${LEASE_DIRECTORY}" 2>/dev/null; then
    printf 'Another writer owns this worktree:\n' >&2
    status_lease >&2
    exit 1
  fi
  chmod 700 "${LEASE_DIRECTORY}"
  local ACQUIRE_COMPLETE=false
  trap 'if [[ "${ACQUIRE_COMPLETE}" != true ]]; then remove_lease_files; fi' EXIT
  umask 077

  git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all >"$(lease_file baseline_status)"
  if [[ -s "$(lease_file baseline_status)" ]]; then
    printf 'Worktree must be clean before acquiring a write lease:\n' >&2
    sed -n '1,200p' "$(lease_file baseline_status)" >&2
    exit 1
  fi

  printf '%s\n' "${TASK_ID}" >"$(lease_file task)"
  printf '%s\n' "${OWNER}" >"$(lease_file owner)"
  printf '%s\n' "$$" >"$(lease_file pid)"
  printf '%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$(lease_file acquired_at)"
  git -C "${MAILCLI_ROOT}" rev-parse HEAD >"$(lease_file baseline_head)"
  printf '%s\n' "$@" | LC_ALL=C sort -u >"$(lease_file allowed_paths)"
  while IFS= read -r RELATIVE_PATH; do
    printf '%s\t%s\n' "$(fingerprint_path "${RELATIVE_PATH}")" "${RELATIVE_PATH}"
  done <"$(lease_file allowed_paths)" >"$(lease_file allowed_fingerprints)"

  local TOKEN
  TOKEN="$(printf '%s:%s:%s:%s' "${TASK_ID}" "${OWNER}" "$$" "${RANDOM}" |
    shasum -a 256 | awk '{print $1}')"
  printf '%s\n' "${TOKEN}" >"$(lease_file token)"
  ACQUIRE_COMPLETE=true
  trap - EXIT
  printf 'write_lease_token=%s\n' "${TOKEN}"
  printf 'write_lease_task=%s\n' "${TASK_ID}"
  printf 'write_lease_head=%s\n' "$(<"$(lease_file baseline_head)")"
}

status_lease() {
  if [[ ! -d "${LEASE_DIRECTORY}" ]]; then
    printf 'write_lease=inactive\n'
    return
  fi
  local NAME
  printf 'write_lease=active\n'
  for NAME in task owner pid acquired_at baseline_head; do
    if [[ -f "$(lease_file "${NAME}")" ]]; then
      printf '%s=%s\n' "${NAME}" "$(<"$(lease_file "${NAME}")")"
    else
      printf '%s=missing\n' "${NAME}"
    fi
  done
}

verify_staged_scope() {
  local BASELINE_HEAD
  local RELATIVE_PATH
  local UNAUTHORIZED=false
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  : >"$(lease_file changed_paths)"
  git -C "${MAILCLI_ROOT}" diff --cached --name-only "${BASELINE_HEAD}" -- |
    LC_ALL=C sort -u >"$(lease_file changed_paths)"
  [[ -s "$(lease_file changed_paths)" ]] || fail "No staged TASK change is available for review"
  while IFS= read -r RELATIVE_PATH; do
    if ! path_is_allowed "${RELATIVE_PATH}"; then
      printf 'Staged path is outside the lease allowlist: %s\n' "${RELATIVE_PATH}" >&2
      UNAUTHORIZED=true
    fi
  done <"$(lease_file changed_paths)"
  [[ "${UNAUTHORIZED}" == false ]] || exit 1

  if ! git -C "${MAILCLI_ROOT}" diff --quiet; then
    fail "Unstaged tracked changes remain; stage the reviewed TASK patch exactly"
  fi
  local UNTRACKED
  UNTRACKED="$(git -C "${MAILCLI_ROOT}" ls-files --others --exclude-standard)"
  [[ -z "${UNTRACKED}" ]] || fail "Untracked non-ignored files remain outside the staged TASK patch"
  git -C "${MAILCLI_ROOT}" diff --cached --check "${BASELINE_HEAD}" --
}

review_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  local BASELINE_HEAD
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "HEAD changed after lease acquisition; stop and inspect the concurrent commit"
  verify_staged_scope

  local RELATIVE_PATH
  local BASELINE_FINGERPRINT
  local CURRENT_FINGERPRINT
  while IFS=$'\t' read -r BASELINE_FINGERPRINT RELATIVE_PATH; do
    CURRENT_FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")"
    if [[ "${CURRENT_FINGERPRINT}" != "${BASELINE_FINGERPRINT}" ]] &&
      ! grep -Fxq -- "${RELATIVE_PATH}" "$(lease_file changed_paths)"; then
      printf 'ignored_allowed_path_changed=%s\n' "${RELATIVE_PATH}"
    fi
  done <"$(lease_file allowed_fingerprints)"

  local REVIEWED_DIGEST
  local PATCH_DIGEST
  REVIEWED_DIGEST="$(allowed_state_digest)"
  PATCH_DIGEST="$(staged_patch_digest)"
  printf '%s\n' "${REVIEWED_DIGEST}" >"$(lease_file reviewed_digest)"
  printf '%s\n' "${PATCH_DIGEST}" >"$(lease_file reviewed_patch_sha256)"
  printf '%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$(lease_file reviewed_at)"
  printf 'reviewed_paths:\n'
  sed 's/^/  /' "$(lease_file changed_paths)"
  git -C "${MAILCLI_ROOT}" diff --cached --stat "${BASELINE_HEAD}" --
  git -C "${MAILCLI_ROOT}" diff --cached "${BASELINE_HEAD}" --
  printf 'reviewed_patch_sha256=%s\n' "${PATCH_DIGEST}"
}

gate_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  [[ -f "$(lease_file reviewed_digest)" ]] || fail "Review the staged TASK patch before the full gate"
  local BASELINE_HEAD
  local CURRENT_DIGEST
  local CURRENT_PATCH_DIGEST
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "HEAD changed after lease acquisition; gate evidence is invalid"
  verify_staged_scope
  CURRENT_DIGEST="$(allowed_state_digest)"
  CURRENT_PATCH_DIGEST="$(staged_patch_digest)"
  [[ "${CURRENT_DIGEST}" == "$(<"$(lease_file reviewed_digest)")" ]] ||
    fail "Allowed file content changed after review; review the patch again"
  [[ "${CURRENT_PATCH_DIGEST}" == "$(<"$(lease_file reviewed_patch_sha256)")" ]] ||
    fail "Staged patch changed after review; review the patch again"

  rm -f "$(lease_file gate_patch_sha256)" "$(lease_file gate_head)" "$(lease_file gate_at)"
  local GATE_STATUS=0
  "${MAILCLI_ROOT}/scripts/tests/test.sh" || GATE_STATUS=$?
  if [[ "${GATE_STATUS}" -ne 0 ]]; then
    printf 'Full gate failed with status %s; commit proof was not recorded\n' "${GATE_STATUS}" >&2
    return "${GATE_STATUS}"
  fi

  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "HEAD changed while the full gate was running"
  verify_staged_scope
  [[ "$(allowed_state_digest)" == "${CURRENT_DIGEST}" ]] ||
    fail "Allowed file content changed while the full gate was running"
  [[ "$(staged_patch_digest)" == "${CURRENT_PATCH_DIGEST}" ]] ||
    fail "Staged patch changed while the full gate was running"
  printf '%s\n' "${CURRENT_PATCH_DIGEST}" >"$(lease_file gate_patch_sha256)"
  printf '%s\n' "${BASELINE_HEAD}" >"$(lease_file gate_head)"
  printf '%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$(lease_file gate_at)"
  printf 'full_gate=passed\n'
  printf 'gated_patch_sha256=%s\n' "${CURRENT_PATCH_DIGEST}"
}

release_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  [[ -f "$(lease_file gate_patch_sha256)" ]] ||
    fail "No successful full-gate evidence exists for this lease"
  local TASK_ID
  local BASELINE_HEAD
  local CURRENT_HEAD
  local PARENT_HEAD
  local COMMIT_SUBJECT
  local COMMITTED_PATCH_DIGEST
  local GATED_PATCH_DIGEST
  local RELATIVE_PATH
  TASK_ID="$(<"$(lease_file task)")"
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  CURRENT_HEAD="$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)"
  PARENT_HEAD="$(git -C "${MAILCLI_ROOT}" rev-parse "${CURRENT_HEAD}^")"
  [[ "${PARENT_HEAD}" == "${BASELINE_HEAD}" ]] ||
    fail "Lease requires exactly one TASK commit whose parent is the acquired HEAD"
  COMMIT_SUBJECT="$(git -C "${MAILCLI_ROOT}" log -1 --format=%s "${CURRENT_HEAD}")"
  [[ "${COMMIT_SUBJECT}" == "TASK ${TASK_ID}: "* ]] ||
    fail "Commit subject must start with TASK ${TASK_ID}:"
  while IFS= read -r RELATIVE_PATH; do
    path_is_allowed "${RELATIVE_PATH}" ||
      fail "Committed path is outside the lease allowlist: ${RELATIVE_PATH}"
  done < <(git -C "${MAILCLI_ROOT}" diff --name-only "${BASELINE_HEAD}" "${CURRENT_HEAD}" --)
  COMMITTED_PATCH_DIGEST="$(commit_patch_digest "${BASELINE_HEAD}" "${CURRENT_HEAD}")"
  GATED_PATCH_DIGEST="$(<"$(lease_file gate_patch_sha256)")"
  [[ "${COMMITTED_PATCH_DIGEST}" == "${GATED_PATCH_DIGEST}" ]] ||
    fail "Committed patch differs from the exact patch that passed the full gate"
  [[ "$(allowed_state_digest)" == "$(<"$(lease_file reviewed_digest)")" ]] ||
    fail "Allowed file content changed after the successful full gate"
  [[ -z "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)" ]] ||
    fail "Worktree is not clean after the TASK commit"
  remove_lease_files
  printf 'write_lease=released\n'
  printf 'task_commit=%s\n' "${CURRENT_HEAD}"
}

abort_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  local BASELINE_HEAD
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "Cannot abort a lease after HEAD changed"
  [[ -z "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)" ]] ||
    fail "Cannot abort a lease while worktree changes remain"
  remove_lease_files
  printf 'write_lease=aborted\n'
}

COMMAND="${1:-}"
case "${COMMAND}" in
  acquire)
    shift
    acquire_lease "$@"
    ;;
  status)
    [[ "$#" -eq 1 ]] || fail "status accepts no additional arguments"
    status_lease
    ;;
  review | gate | release | abort)
    [[ "$#" -eq 2 ]] || fail "${COMMAND} requires exactly one lease token"
    "${COMMAND}_lease" "$2"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

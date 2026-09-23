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
    '  manage-write-lease.sh private-proof TOKEN SNAPSHOT PATH [PATH...]' \
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
    allowed_paths allowed_fingerprints private_task_fingerprints \
    ignored_asset_fingerprints \
    reviewed_digest reviewed_patch_sha256 \
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
  local DIGEST
  if [[ -L "${ABSOLUTE_PATH}" ]]; then
    DIGEST="$(readlink "${ABSOLUTE_PATH}")" ||
      fail "Could not read symlink: ${RELATIVE_PATH}"
    printf 'symlink:%s' "${DIGEST}"
  elif [[ -f "${ABSOLUTE_PATH}" ]]; then
    DIGEST="$(shasum -a 256 "${ABSOLUTE_PATH}" | awk '{print $1}')" ||
      fail "Could not fingerprint path: ${RELATIVE_PATH}"
    [[ "${DIGEST}" =~ ^[0-9a-f]{64}$ ]] ||
      fail "Invalid fingerprint for path: ${RELATIVE_PATH}"
    printf 'file:%s' "${DIGEST}"
  elif [[ -d "${ABSOLUTE_PATH}" ]]; then
    printf 'directory'
  elif [[ -e "${ABSOLUTE_PATH}" ]]; then
    printf 'unsupported'
  else
    printf 'absent'
  fi
}

path_is_allowed() {
  grep -Fxq -- "$1" "$(lease_file allowed_paths)"
}

private_task_snapshot() {
  local ABSOLUTE_PATH
  local RELATIVE_PATH
  local FINGERPRINT
  FINGERPRINT="$(fingerprint_path docs/tasks.md)" || fail "Could not fingerprint task board"
  printf 'docs/tasks.md\t%s\n' "${FINGERPRINT}"
  FINGERPRINT="$(fingerprint_path docs/tasks)" || fail "Could not fingerprint task directory"
  if [[ "${FINGERPRINT}" == directory ]]; then
    FINGERPRINT=unsupported
  fi
  printf 'docs/tasks\t%s\n' "${FINGERPRINT}"
  [[ -d "${MAILCLI_ROOT}/docs/tasks" ]] || return 0
  while IFS= read -r -d '' ABSOLUTE_PATH; do
    RELATIVE_PATH="${ABSOLUTE_PATH#"${MAILCLI_ROOT}/"}"
    [[ "${RELATIVE_PATH}" != *$'\n'* && "${RELATIVE_PATH}" != *$'\t'* ]] ||
      fail "Private task path contains tabs or newlines"
    FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint private task path"
    printf '%s\t%s\n' "${RELATIVE_PATH}" "${FINGERPRINT}"
  done < <(find "${MAILCLI_ROOT}/docs/tasks" -mindepth 1 ! -type d -print0)
}

require_snapshot_path() {
  local RELATIVE_PATH="$1"
  [[ "${RELATIVE_PATH}" != *$'\n'* && "${RELATIVE_PATH}" != *$'\t'* ]] ||
    fail "Ignored asset path contains tabs or newlines"
}

ignored_asset_snapshot() {
  local RELATIVE_PATH
  local ABSOLUTE_PATH
  local FINGERPRINT
  git -C "${MAILCLI_ROOT}" ls-files --others --ignored --exclude-standard -z |
    while IFS= read -r -d '' RELATIVE_PATH; do
      case "${RELATIVE_PATH}" in
        docs/tasks.md | docs/tasks/*) continue ;;
      esac
      require_snapshot_path "${RELATIVE_PATH}"
      FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
        fail "Could not fingerprint ignored file"
      [[ "${FINGERPRINT}" != unsupported ]] ||
        fail "Ignored asset has an unsupported file type: ${RELATIVE_PATH}"
      printf '%s\t%s\n' "${RELATIVE_PATH}" "${FINGERPRINT}"
    done || fail "Could not inventory ignored files"

  git -C "${MAILCLI_ROOT}" ls-files --others --ignored --exclude-standard --directory -z |
    while IFS= read -r -d '' RELATIVE_PATH; do
      RELATIVE_PATH="${RELATIVE_PATH%/}"
      [[ -d "${MAILCLI_ROOT}/${RELATIVE_PATH}" &&
        ! -L "${MAILCLI_ROOT}/${RELATIVE_PATH}" ]] || continue
      find "${MAILCLI_ROOT}/${RELATIVE_PATH}" -type d -print0 |
        while IFS= read -r -d '' ABSOLUTE_PATH; do
          RELATIVE_PATH="${ABSOLUTE_PATH#"${MAILCLI_ROOT}/"}"
          require_snapshot_path "${RELATIVE_PATH}"
          FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
            fail "Could not fingerprint ignored directory"
          printf '%s\t%s\n' "${RELATIVE_PATH}" "${FINGERPRINT}"
        done || fail "Could not inventory ignored directories"
    done || fail "Could not inventory ignored directory roots"
}

verify_private_task_scope() {
  local BASELINE
  local CURRENT
  local DIFF_STATUS
  local OUT_OF_SCOPE
  BASELINE="$(lease_file private_task_fingerprints)"
  [[ -f "${BASELINE}" ]] || fail "Private task baseline is missing"
  CURRENT="$(private_task_snapshot | LC_ALL=C sort)"
  if OUT_OF_SCOPE="$(diff -u \
    <(awk -F '\t' 'NR == FNR { allowed[$0] = 1; next } !($1 in allowed)' \
      "$(lease_file allowed_paths)" "${BASELINE}") \
    <(printf '%s\n' "${CURRENT}" |
      awk -F '\t' 'NR == FNR { allowed[$0] = 1; next } !($1 in allowed)' \
        "$(lease_file allowed_paths)" -))"; then
    return 0
  else
    DIFF_STATUS=$?
  fi
  [[ "${DIFF_STATUS}" -eq 1 ]] || fail "Could not compare private task files"
  printf 'Private task path changed outside the lease allowlist:\n%s\n' \
    "${OUT_OF_SCOPE}" >&2
  return 1
}

verify_ignored_asset_scope() {
  local BASELINE
  local CURRENT
  local DIFF_STATUS
  local OUT_OF_SCOPE
  BASELINE="$(lease_file ignored_asset_fingerprints)"
  if [[ ! -f "${BASELINE}" ]]; then
    local BASELINE_HEAD
    local BASELINE_SCRIPT
    BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
    BASELINE_SCRIPT="$(git -C "${MAILCLI_ROOT}" show \
      "${BASELINE_HEAD}:scripts/utils/manage-write-lease.sh")" ||
      fail "Could not inspect baseline lease script"
    if [[ "${BASELINE_SCRIPT}" == *ignored_asset_fingerprints* ]]; then
      fail "Ignored asset baseline is missing"
    fi
    printf 'ignored_asset_scope=legacy_lease\n' >&2
    return 0
  fi
  CURRENT="$(ignored_asset_snapshot | LC_ALL=C sort)" ||
    fail "Could not inventory ignored assets"
  if OUT_OF_SCOPE="$(diff -u \
    <(awk -F '\t' 'NR == FNR { allowed[$0] = 1; next } !($1 in allowed)' \
      "$(lease_file allowed_paths)" "${BASELINE}") \
    <(printf '%s\n' "${CURRENT}" |
      awk -F '\t' 'NR == FNR { allowed[$0] = 1; next } !($1 in allowed)' \
        "$(lease_file allowed_paths)" -))"; then
    return 0
  else
    DIFF_STATUS=$?
  fi
  [[ "${DIFF_STATUS}" -eq 1 ]] || fail "Could not compare ignored assets"
  printf 'Ignored asset changed outside the lease allowlist:\n%s\n' \
    "${OUT_OF_SCOPE}" >&2
  return 1
}

allowed_state_digest() {
  local RELATIVE_PATH
  local FINGERPRINT
  {
    printf 'head=%s\n' "$(<"$(lease_file baseline_head)")"
    while IFS= read -r RELATIVE_PATH; do
      FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
        fail "Could not fingerprint allowed path"
      printf '%s\t%s\n' "${RELATIVE_PATH}" "${FINGERPRINT}"
    done <"$(lease_file allowed_paths)"
  } | shasum -a 256 | awk '{print $1}'
}

report_allowed_path_changes() {
  local BASELINE_FINGERPRINT
  local CURRENT_FINGERPRINT
  local RELATIVE_PATH
  while IFS=$'\t' read -r BASELINE_FINGERPRINT RELATIVE_PATH; do
    CURRENT_FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint allowed path"
    if [[ "${CURRENT_FINGERPRINT}" != "${BASELINE_FINGERPRINT}" ]]; then
      printf 'allowed_path_change\t%s\t%s\t%s\n' \
        "${RELATIVE_PATH}" "${BASELINE_FINGERPRINT}" "${CURRENT_FINGERPRINT}"
    fi
  done <"$(lease_file allowed_fingerprints)"
}

source_task_manifest() {
  local RELATIVE_PATH
  local DIGEST
  [[ -f "${MAILCLI_ROOT}/docs/tasks.md" &&
    ! -L "${MAILCLI_ROOT}/docs/tasks.md" ]] || fail "Task board is not a regular file"
  [[ -d "${MAILCLI_ROOT}/docs/tasks" &&
    ! -L "${MAILCLI_ROOT}/docs/tasks" ]] || fail "Task directory is not a real directory"
  [[ -z "$(find "${MAILCLI_ROOT}/docs/tasks" ! -type d ! -type f -print -quit)" ]] ||
    fail "Task history contains an unsupported path"
  [[ -z "$(find "${MAILCLI_ROOT}/docs/tasks" -type f ! -name '*.md' -print -quit)" ]] ||
    fail "Task history contains a non-Markdown file"
  {
    printf '%s\n' docs/tasks.md
    find "${MAILCLI_ROOT}/docs/tasks" -type f -name '*.md' -print |
      sed "s#^${MAILCLI_ROOT}/##"
  } | LC_ALL=C sort | while IFS= read -r RELATIVE_PATH; do
    [[ "${RELATIVE_PATH}" == docs/tasks.md ||
      "${RELATIVE_PATH}" =~ ^docs/tasks/(done/)?[0-9]{3}-[a-z0-9-]+\.md$ ]] ||
      fail "Invalid task history path: ${RELATIVE_PATH}"
    DIGEST="$(shasum -a 256 "${MAILCLI_ROOT}/${RELATIVE_PATH}" | awk '{print $1}')" ||
      fail "Could not hash task history path: ${RELATIVE_PATH}"
    [[ "${DIGEST}" =~ ^[0-9a-f]{64}$ ]] || fail "Invalid task history hash"
    printf '%s\t%s\n' "${DIGEST}" "${RELATIVE_PATH}"
  done
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
    /* | . | ./* | */. | */./* | *//* | .. | ../* | */.. | */../* | */)
      fail "Allowed path must stay inside the worktree: ${RELATIVE_PATH}"
      ;;
  esac
  [[ "${RELATIVE_PATH}" != *$'\n'* && "${RELATIVE_PATH}" != *$'\t'* ]] ||
    fail "Allowed path must not contain tabs or newlines"
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
  local FINGERPRINT
  while IFS= read -r RELATIVE_PATH; do
    FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint allowed path"
    printf '%s\t%s\n' "${FINGERPRINT}" "${RELATIVE_PATH}"
  done <"$(lease_file allowed_paths)" >"$(lease_file allowed_fingerprints)"
  private_task_snapshot | LC_ALL=C sort >"$(lease_file private_task_fingerprints)"
  ignored_asset_snapshot | LC_ALL=C sort >"$(lease_file ignored_asset_fingerprints)"

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
  verify_private_task_scope
  verify_ignored_asset_scope
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
    CURRENT_FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint allowed path"
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
  local HARNESS_DIR
  local HARNESS_SCRIPT
  local HARNESS_CAPABLE=true
  HARNESS_DIR="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-gate-harness.XXXXXX")"
  if ! git -C "${MAILCLI_ROOT}" archive "${BASELINE_HEAD}" scripts/tests |
    tar -x -C "${HARNESS_DIR}"; then
    rm -rf "${HARNESS_DIR}"
    fail "Could not extract the baseline gate harness from ${BASELINE_HEAD}"
  fi
  while IFS= read -r -d '' HARNESS_SCRIPT; do
    if ! grep -q 'MAILCLI_ROOT:-' "${HARNESS_SCRIPT}"; then
      HARNESS_CAPABLE=false
      break
    fi
  done < <(find "${HARNESS_DIR}/scripts/tests" -type f -name 'test*.sh' -print0)
  if [[ "${HARNESS_CAPABLE}" == true &&
    -x "${HARNESS_DIR}/scripts/tests/test.sh" ]]; then
    MAILCLI_ROOT="${MAILCLI_ROOT}" \
      "${HARNESS_DIR}/scripts/tests/test.sh" || GATE_STATUS=$?
    rm -rf "${HARNESS_DIR}"
    printf 'gate_harness=baseline\n'
  else
    rm -rf "${HARNESS_DIR}"
    printf 'gate_harness=worktree_transitional\n' >&2
    "${MAILCLI_ROOT}/scripts/tests/test.sh" || GATE_STATUS=$?
  fi
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
  verify_private_task_scope
  verify_ignored_asset_scope
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

private_proof_lease() {
  [[ "$#" -ge 4 ]] || fail "private-proof requires a token, snapshot, and expected changed paths"
  local TOKEN="$1"
  local SNAPSHOT_DIRECTORY="$2"
  shift 2
  require_token "${TOKEN}"
  [[ -f "$(lease_file ignored_asset_fingerprints)" ]] ||
    fail "Private closure requires an ignored-asset baseline"
  verify_private_task_scope
  verify_ignored_asset_scope

  local TASK_ID
  local BASELINE_HEAD
  TASK_ID="$(<"$(lease_file task)")"
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "HEAD changed during private closure"
  [[ -z "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)" ]] ||
    fail "Tracked worktree is not clean for private closure"

  local EXPECTED_PATHS
  local UNIQUE_PATHS
  local RELATIVE_PATH
  EXPECTED_PATHS="$(printf '%s\n' "$@" | LC_ALL=C sort)"
  UNIQUE_PATHS="$(printf '%s\n' "$@" | LC_ALL=C sort -u)"
  [[ "${EXPECTED_PATHS}" == "${UNIQUE_PATHS}" ]] || fail "Expected paths contain duplicates"
  while IFS= read -r RELATIVE_PATH; do
    validate_allowed_path "${RELATIVE_PATH}"
    path_is_allowed "${RELATIVE_PATH}" ||
      fail "Expected private path is outside the lease: ${RELATIVE_PATH}"
  done <<<"${EXPECTED_PATHS}"

  local CHANGE_ROWS
  local ACTUAL_PATHS
  CHANGE_ROWS="$(report_allowed_path_changes)" || fail "Could not compare allowed paths"
  [[ -n "${CHANGE_ROWS}" ]] || fail "No private path changed"
  ACTUAL_PATHS="$(printf '%s\n' "${CHANGE_ROWS}" | cut -f 2 | LC_ALL=C sort)"
  [[ "${ACTUAL_PATHS}" == "${EXPECTED_PATHS}" ]] ||
    fail "Changed paths differ from the expected private deliverable"

  local DONE_LINE
  local DETAIL_PATH
  local SOURCE_PATH
  DONE_LINE="$(grep -E "^- \\[x\\] ${TASK_ID} .+ -> tasks/done/${TASK_ID}-[a-z0-9-]+\\.md$" \
    "${MAILCLI_ROOT}/docs/tasks.md" || true)"
  [[ -n "${DONE_LINE}" && "${DONE_LINE}" != *$'\n'* ]] ||
    fail "Task board lacks one canonical Done entry"
  [[ "$(grep -Ec "^- \\[[^]]\\] ${TASK_ID} " "${MAILCLI_ROOT}/docs/tasks.md")" -eq 1 ]] ||
    fail "Task board contains duplicate task entries"
  DETAIL_PATH="docs/${DONE_LINE##* -> }"
  SOURCE_PATH="docs/tasks/${DETAIL_PATH##*/}"
  [[ -f "${MAILCLI_ROOT}/${DETAIL_PATH}" &&
    ! -L "${MAILCLI_ROOT}/${DETAIL_PATH}" &&
    ! -e "${MAILCLI_ROOT}/${SOURCE_PATH}" &&
    ! -L "${MAILCLI_ROOT}/${SOURCE_PATH}" ]] ||
    fail "Task detail was not archived by a true path move"
  for RELATIVE_PATH in docs/tasks.md "${SOURCE_PATH}" "${DETAIL_PATH}"; do
    grep -Fxq -- "${RELATIVE_PATH}" <<<"${EXPECTED_PATHS}" ||
      fail "Private closure path is missing from expected changes: ${RELATIVE_PATH}"
  done
  local CHANGE_LABEL
  local BEFORE_FINGERPRINT
  local AFTER_FINGERPRINT
  local MOVE_DESTINATION
  local DESTINATION_CHANGE
  while IFS=$'\t' read -r CHANGE_LABEL RELATIVE_PATH \
    BEFORE_FINGERPRINT AFTER_FINGERPRINT; do
    [[ "${RELATIVE_PATH}" != "${SOURCE_PATH}" &&
      "${RELATIVE_PATH}" =~ ^docs/tasks/[0-9]{3}-[a-z0-9-]+\.md$ &&
      "${AFTER_FINGERPRINT}" == absent ]] || continue
    MOVE_DESTINATION="docs/tasks/done/${RELATIVE_PATH##*/}"
    DESTINATION_CHANGE="$(printf '%s\n' "${CHANGE_ROWS}" |
      awk -F '\t' -v target="${MOVE_DESTINATION}" \
        '$2 == target { print $3 "\t" $4 }')"
    [[ "${BEFORE_FINGERPRINT}" == file:* &&
      "${DESTINATION_CHANGE}" == $'absent\t'"${BEFORE_FINGERPRINT}" ]] ||
      fail "Archived task move changed content or lost its destination: ${RELATIVE_PATH}"
  done <<<"${CHANGE_ROWS}"
  grep -Eq "^# TASK ${TASK_ID}: .+$" "${MAILCLI_ROOT}/${DETAIL_PATH}" ||
    fail "Task detail heading does not match its board ID"
  for RELATIVE_PATH in Why Acceptance Sub-Tasks Notes Deviations; do
    grep -Fxq "## ${RELATIVE_PATH}" "${MAILCLI_ROOT}/${DETAIL_PATH}" ||
      fail "Task detail is missing section: ${RELATIVE_PATH}"
  done
  awk '/^## Sub-Tasks$/ { inside=1; next }
       /^## / && inside { exit }
       inside && /^- \[x\] / { complete++ }
       inside && /^- \[[^x]\] / { incomplete=1 }
       END { exit !(complete > 0 && !incomplete) }' \
    "${MAILCLI_ROOT}/${DETAIL_PATH}" || fail "Task detail has unfinished sub-tasks"

  [[ "${SNAPSHOT_DIRECTORY}" == /* &&
    "${SNAPSHOT_DIRECTORY}" != *$'\n'* &&
    "${SNAPSHOT_DIRECTORY}" != *$'\t'* ]] || fail "Snapshot path must be absolute and one line"
  "${MAILCLI_ROOT}/scripts/utils/export-task-history.sh" verify \
    "${SNAPSHOT_DIRECTORY}" >/dev/null || fail "Task snapshot is invalid"
  SNAPSHOT_DIRECTORY="$(cd "${SNAPSHOT_DIRECTORY}" && pwd -P)"
  [[ "${SNAPSHOT_DIRECTORY}" != "${MAILCLI_ROOT}" &&
    "${SNAPSHOT_DIRECTORY}" != "${MAILCLI_ROOT}/"* ]] ||
    fail "Private closure snapshot must be outside the repository"
  local SOURCE_MANIFEST
  local SNAPSHOT_MANIFEST
  SOURCE_MANIFEST="$(source_task_manifest)" || fail "Could not inventory current task history"
  SNAPSHOT_MANIFEST="$(<"${SNAPSHOT_DIRECTORY}/MANIFEST.sha256")"
  [[ "${SOURCE_MANIFEST}" == "${SNAPSHOT_MANIFEST}" ]] ||
    fail "Task snapshot is stale or has the wrong source scope"

  verify_private_task_scope
  verify_ignored_asset_scope
  [[ "$(source_task_manifest)" == "${SOURCE_MANIFEST}" ]] ||
    fail "Task history changed during private proof"
  [[ "$(report_allowed_path_changes)" == "${CHANGE_ROWS}" ]] ||
    fail "Allowed path identities changed during private proof"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" &&
    -z "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)" ]] ||
    fail "Git state changed during private proof"

  local RECEIPT_PATH="${SNAPSHOT_DIRECTORY}.task-${TASK_ID}.receipt"
  [[ ! -e "${RECEIPT_PATH}" && ! -L "${RECEIPT_PATH}" ]] ||
    fail "Private closure receipt already exists"
  local RECEIPT_TEMP
  RECEIPT_TEMP="$(mktemp "${RECEIPT_PATH}.tmp.XXXXXX")" ||
    fail "Could not allocate private closure receipt"
  trap 'rm -f -- "${RECEIPT_TEMP}"' EXIT
  chmod 600 "${RECEIPT_TEMP}"
  {
    printf 'private_closure=verified\ntask=%s\nowner=%s\nhead=%s\n' \
      "${TASK_ID}" "$(<"$(lease_file owner)")" "${BASELINE_HEAD}"
    printf 'snapshot=%s\nmanifest_sha256=%s\n' "${SNAPSHOT_DIRECTORY}" \
      "$(shasum -a 256 "${SNAPSHOT_DIRECTORY}/MANIFEST.sha256" | awk '{print $1}')"
    printf '%s\n' "${CHANGE_ROWS}"
  } >"${RECEIPT_TEMP}"
  ln "${RECEIPT_TEMP}" "${RECEIPT_PATH}" || fail "Could not publish private closure receipt"
  rm -f -- "${RECEIPT_TEMP}"
  trap - EXIT
  cat "${RECEIPT_PATH}"
  printf 'private_receipt_path=%s\n' "${RECEIPT_PATH}"
}

abort_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  verify_private_task_scope
  verify_ignored_asset_scope
  local BASELINE_HEAD
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "Cannot abort a lease after HEAD changed"
  [[ -z "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)" ]] ||
    fail "Cannot abort a lease while worktree changes remain"
  report_allowed_path_changes
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
  private-proof)
    shift
    private_proof_lease "$@"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

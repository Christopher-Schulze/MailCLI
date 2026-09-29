#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_ROOT="${MAILCLI_WRITE_ROOT:-${SCRIPT_ROOT}}"

usage() {
  printf '%s\n' \
    'Usage:' \
    '  manage-write-lease.sh acquire TASK_ID[,TASK_ID...] OWNER PATH [PATH...]' \
    '  manage-write-lease.sh extend TOKEN PATH [PATH...]' \
    '  manage-write-lease.sh status' \
    '  manage-write-lease.sh review TOKEN [--diff]' \
    '  manage-write-lease.sh gate TOKEN [--fast|--full|--checks REGISTERED_PATH... [--expect-baseline-failure PATH EXACT_FINAL_LINE]]' \
    '  manage-write-lease.sh precommit' \
    '  manage-write-lease.sh push-check' \
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
[[ "${GIT_ROOT}" -ef "${MAILCLI_ROOT}" ]] ||
  fail "Write root must be the worktree root: ${GIT_ROOT}"
MAILCLI_ROOT="${GIT_ROOT}"
GIT_DIRECTORY="$(git -C "${MAILCLI_ROOT}" rev-parse --absolute-git-dir)"
LEASE_DIRECTORY="${GIT_DIRECTORY}/mailcli-write-lease"
PRIMARY_ROOT="$(git -C "${MAILCLI_ROOT}" worktree list --porcelain | sed -n '1s/^worktree //p')"
PRIMARY_ROOT="$(cd "${PRIMARY_ROOT}" && pwd -P)"

lease_file() {
  printf '%s/%s\n' "${LEASE_DIRECTORY}" "$1"
}

remove_lease_files() {
  local NAME
  for NAME in task owner_session token acquired_at baseline_head \
    allowed_paths allowed_fingerprints allowed_paths.next allowed_fingerprints.next \
    ignored_asset_fingerprints \
    reviewed_digest reviewed_patch_sha256 lint_identity \
    gate_patch_sha256 gate_index_tree \
    gate_harness gate_tier gate_shell_receipts changed_paths; do
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
        docs/tasks.md | docs/tasks | docs/tasks/* | graphify-out/*) continue ;;
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
      case "${RELATIVE_PATH}" in docs/tasks | docs/tasks/* | graphify-out | graphify-out/*) continue ;; esac
      [[ -d "${MAILCLI_ROOT}/${RELATIVE_PATH}" &&
        ! -L "${MAILCLI_ROOT}/${RELATIVE_PATH}" ]] || continue
      find "${MAILCLI_ROOT}/${RELATIVE_PATH}" -type d -print0 |
        while IFS= read -r -d '' ABSOLUTE_PATH; do
          RELATIVE_PATH="${ABSOLUTE_PATH#"${MAILCLI_ROOT}/"}"
          case "${RELATIVE_PATH}" in docs/tasks | docs/tasks/*) continue ;; esac
          require_snapshot_path "${RELATIVE_PATH}"
          FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
            fail "Could not fingerprint ignored directory"
          printf '%s\t%s\n' "${RELATIVE_PATH}" "${FINGERPRINT}"
        done || fail "Could not inventory ignored directories"
    done || fail "Could not inventory ignored directory roots"
}


verify_ignored_asset_scope() {
  local BASELINE
  local CURRENT
  local DIFF_STATUS
  local OUT_OF_SCOPE
  BASELINE="$(lease_file ignored_asset_fingerprints)"
  [[ -f "${BASELINE}" ]] || fail "Ignored asset baseline is missing"
  CURRENT="$(ignored_asset_snapshot | LC_ALL=C sort)" ||
    fail "Could not inventory ignored assets"
  if OUT_OF_SCOPE="$(diff -u \
    <(awk -F '\t' 'NR == FNR { allowed[$0] = 1; next }
      $1 == "docs/tasks.md" || $1 == "docs/tasks" || index($1, "docs/tasks/") == 1 { next }
      $1 == "graphify-out" || index($1, "graphify-out/") == 1 { next }
      !($1 in allowed)' \
      "$(lease_file allowed_paths)" "${BASELINE}") \
    <(printf '%s' "${CURRENT}" |
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
  local PARENT="${RELATIVE_PATH%/*}"
  if [[ "${PARENT}" != "${RELATIVE_PATH}" ]]; then
    while [[ "${PARENT}" != . ]]; do
      [[ ! -L "${MAILCLI_ROOT}/${PARENT}" ]] || fail "Allowed path has a symlink parent: ${RELATIVE_PATH}"
      [[ "${PARENT}" == */* ]] || break
      PARENT="${PARENT%/*}"
    done
  fi
  if [[ -d "${MAILCLI_ROOT}/${RELATIVE_PATH}" && ! -L "${MAILCLI_ROOT}/${RELATIVE_PATH}" &&
    -n "$(git -C "${MAILCLI_ROOT}" ls-files -- ":(literal)${RELATIVE_PATH}/")" ]]; then
    fail "Lease paths must name tracked files, not a tracked directory: ${RELATIVE_PATH}"
  fi
}

normalize_task_ids() {
  local PROVIDED_IDS="$1"
  local SORTED_IDS
  local UNIQUE_IDS
  local TASK_IDS
  [[ "${PROVIDED_IDS}" =~ ^[0-9]{3}(,[0-9]{3})*$ ]] ||
    fail "TASK IDs must be comma-separated three-digit IDs"
  IFS=',' read -r -a TASK_IDS <<<"${PROVIDED_IDS}"
  SORTED_IDS="$(printf '%s\n' "${TASK_IDS[@]}" | LC_ALL=C sort)"
  UNIQUE_IDS="$(printf '%s\n' "${TASK_IDS[@]}" | LC_ALL=C sort -u)"
  [[ "${SORTED_IDS}" == "${UNIQUE_IDS}" ]] || fail "TASK IDs contain duplicates"
  printf '%s\n' "${SORTED_IDS}" | paste -sd ',' -
}

validate_task_group() {
  local TASK_IDS="$1"
  local TASK_ID
  local MATCH_COUNT
  local ENTRY_COUNT
  local BOARD_ENTRY
  local DETAIL_PATH
  local BOARD_PATH="${MAILCLI_ROOT}/docs/tasks.md"
  [[ "${TASK_IDS}" == *,* ]] || return 0
  [[ -f "${BOARD_PATH}" && ! -L "${BOARD_PATH}" ]] ||
    fail "A grouped lease requires the local task board"
  while IFS= read -r TASK_ID; do
    MATCH_COUNT="$(grep -Ec \
      "^- \\[[ ~]\\] ${TASK_ID} .+ -> tasks/${TASK_ID}-[a-z0-9-]+\\.md$" \
      "${BOARD_PATH}" || true)"
    [[ "${MATCH_COUNT}" == 1 ]] ||
      fail "Grouped TASK ${TASK_ID} must have one open local board entry"
    ENTRY_COUNT="$(grep -Ec "^- \\[[^]]\\] ${TASK_ID} " "${BOARD_PATH}" || true)"
    [[ "${ENTRY_COUNT}" == 1 ]] || fail "Grouped TASK ${TASK_ID} has duplicate board entries"
    BOARD_ENTRY="$(grep -E \
      "^- \\[[ ~]\\] ${TASK_ID} .+ -> tasks/${TASK_ID}-[a-z0-9-]+\\.md$" "${BOARD_PATH}")"
    DETAIL_PATH="${MAILCLI_ROOT}/docs/${BOARD_ENTRY##* -> }"
    [[ -f "${DETAIL_PATH}" && ! -L "${DETAIL_PATH}" ]] ||
      fail "Grouped TASK ${TASK_ID} lacks a regular detail file"
    grep -Eq "^# TASK ${TASK_ID}: .+$" "${DETAIL_PATH}" ||
      fail "Grouped TASK ${TASK_ID} detail has the wrong ID"
  done < <(printf '%s\n' "${TASK_IDS}" | tr ',' '\n')
}

acquire_lease() {
  [[ "$#" -ge 3 ]] || {
    usage >&2
    exit 2
  }
  local TASK_ID="$1"
  local OWNER="$2"
  shift 2
  TASK_ID="$(normalize_task_ids "${TASK_ID}")"
  [[ -n "${OWNER}" && "${OWNER}" != *$'\n'* ]] || fail "OWNER must be one line"
  local RELATIVE_PATH
  [[ "${MAILCLI_ROOT}" == "${PRIMARY_ROOT}" ]] || fail 'Write leases are acquired only in the primary worktree'
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
  trap 'if [[ "${ACQUIRE_COMPLETE:-false}" != true ]]; then remove_lease_files; fi' EXIT
  umask 077
  validate_task_group "${TASK_ID}"

  local BASELINE_STATUS
  BASELINE_STATUS="$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)"
  if [[ -n "${BASELINE_STATUS}" ]]; then
    printf 'Worktree must be clean before acquiring a write lease:\n' >&2
    printf '%s\n' "${BASELINE_STATUS}" >&2
    exit 1
  fi

  printf '%s\n' "${TASK_ID}" >"$(lease_file task)"
  printf '%s\n' "${OWNER}" >"$(lease_file owner_session)"
  printf '%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$(lease_file acquired_at)"
  git -C "${MAILCLI_ROOT}" rev-parse HEAD >"$(lease_file baseline_head)"
  printf '%s\n' "$@" | LC_ALL=C sort -u >"$(lease_file allowed_paths)"
  local FINGERPRINT
  while IFS= read -r RELATIVE_PATH; do
    FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint allowed path"
    printf '%s\t%s\n' "${FINGERPRINT}" "${RELATIVE_PATH}"
  done <"$(lease_file allowed_paths)" >"$(lease_file allowed_fingerprints)"
  ignored_asset_snapshot | LC_ALL=C sort >"$(lease_file ignored_asset_fingerprints)"

  local TOKEN
  TOKEN="$(od -v -An -N32 -tx1 /dev/urandom | tr -d '[:space:]')"
  [[ "${TOKEN}" =~ ^[0-9a-f]{64}$ ]] || fail 'Could not read 32 random token bytes'
  printf '%s\n' "${TOKEN}" >"$(lease_file token)"
  ACQUIRE_COMPLETE=true
  trap - EXIT
  printf 'write_lease_token=%s\n' "${TOKEN}"
  printf 'write_lease_task=%s\n' "${TASK_ID}"
  printf 'write_lease_head=%s\n' "$(<"$(lease_file baseline_head)")"
}

extend_lease() {
  [[ "$#" -ge 2 ]] || {
    usage >&2
    exit 2
  }
  local TOKEN="$1"
  shift
  require_token "${TOKEN}"
  local RELATIVE_PATH
  for RELATIVE_PATH in "$@"; do
    validate_allowed_path "${RELATIVE_PATH}"
    ! path_is_allowed "${RELATIVE_PATH}" || fail "Path is already leased: ${RELATIVE_PATH}"
    [[ -z "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all -- ":(literal)${RELATIVE_PATH}")" ]] ||
      fail "Path changed before it was leased: ${RELATIVE_PATH}"
  done
  umask 077
  local NEXT_PATHS NEXT_FINGERPRINTS FINGERPRINT
  NEXT_PATHS="$(lease_file allowed_paths.next)"
  NEXT_FINGERPRINTS="$(lease_file allowed_fingerprints.next)"
  { cat "$(lease_file allowed_paths)"; printf '%s\n' "$@"; } | LC_ALL=C sort -u >"${NEXT_PATHS}"
  while IFS= read -r RELATIVE_PATH; do
    FINGERPRINT="$(awk -F '\t' -v path="${RELATIVE_PATH}" '$2 == path { print $1 }' "$(lease_file allowed_fingerprints)")"
    [[ -n "${FINGERPRINT}" ]] || FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint allowed path"
    printf '%s\t%s\n' "${FINGERPRINT}" "${RELATIVE_PATH}"
  done <"${NEXT_PATHS}" >"${NEXT_FINGERPRINTS}"
  mv "${NEXT_FINGERPRINTS}" "$(lease_file allowed_fingerprints)"
  mv "${NEXT_PATHS}" "$(lease_file allowed_paths)"
  # Earlier review and gate evidence covered the smaller scope.
  rm -f "$(lease_file reviewed_digest)" "$(lease_file reviewed_patch_sha256)" \
    "$(lease_file gate_patch_sha256)" "$(lease_file gate_index_tree)" "$(lease_file gate_harness)" \
    "$(lease_file gate_tier)" "$(lease_file gate_shell_receipts)"
  printf 'write_lease_extended=%s\n' "$@"
}

status_lease() {
  if [[ ! -d "${LEASE_DIRECTORY}" ]]; then
    printf 'write_lease=inactive\n'
    return
  fi
  local NAME
  printf 'write_lease=active\n'
  for NAME in task owner_session acquired_at baseline_head; do
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
  local OUTPUT_MODE="${2:-}"
  require_token "${TOKEN}"
  local BASELINE_HEAD
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "HEAD changed after lease acquisition; stop and inspect the concurrent commit"
  verify_staged_scope

  local INDEX_TREE
  local LINT_REQUIRED=false
  local PATH_NAME
  while IFS= read -r PATH_NAME; do
    case "${PATH_NAME}" in *.go | go.mod | go.sum | .golangci.yml) LINT_REQUIRED=true ;; esac
    [[ "${PATH_NAME}" != *.sh || ! -f "${MAILCLI_ROOT}/${PATH_NAME}" ]] || bash -n "${MAILCLI_ROOT}/${PATH_NAME}"
  done <"$(lease_file changed_paths)"
  if [[ "${LINT_REQUIRED}" == true ]]; then
    INDEX_TREE="$(git -C "${MAILCLI_ROOT}" write-tree)"
    "${MAILCLI_ROOT}/scripts/utils/run-fast-gate.sh" "${MAILCLI_ROOT}" "${BASELINE_HEAD}" "${INDEX_TREE}" --lint-only "$(lease_file lint_identity)"
    [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" &&
      "$(git -C "${MAILCLI_ROOT}" write-tree)" == "${INDEX_TREE}" ]] || fail 'Review source changed during lint'
    verify_staged_scope
  fi

  local RELATIVE_PATH
  local BASELINE_FINGERPRINT
  local CURRENT_FINGERPRINT
  local IGNORED_CHANGED_COUNT=0
  while IFS=$'\t' read -r BASELINE_FINGERPRINT RELATIVE_PATH; do
    CURRENT_FINGERPRINT="$(fingerprint_path "${RELATIVE_PATH}")" ||
      fail "Could not fingerprint allowed path"
    if [[ "${CURRENT_FINGERPRINT}" != "${BASELINE_FINGERPRINT}" ]] &&
      ! grep -Fxq -- "${RELATIVE_PATH}" "$(lease_file changed_paths)"; then
      IGNORED_CHANGED_COUNT=$((IGNORED_CHANGED_COUNT + 1))
      if [[ "${OUTPUT_MODE}" == --diff ]]; then
        printf 'ignored_allowed_path_changed=%s\n' "${RELATIVE_PATH}"
      fi
    fi
  done <"$(lease_file allowed_fingerprints)"

  local REVIEWED_DIGEST
  local PATCH_DIGEST
  REVIEWED_DIGEST="$(allowed_state_digest)"
  PATCH_DIGEST="$(staged_patch_digest)"
  printf '%s\n' "${REVIEWED_DIGEST}" >"$(lease_file reviewed_digest)"
  printf '%s\n' "${PATCH_DIGEST}" >"$(lease_file reviewed_patch_sha256)"
  local PATH_COUNT
  local OMITTED_COUNT=0
  PATH_COUNT="$(awk 'END { print NR }' "$(lease_file changed_paths)")"
  [[ "${PATH_COUNT}" -le 8 ]] || OMITTED_COUNT=$((PATH_COUNT - 8))
  printf 'reviewed_paths_count=%s\nignored_allowed_paths_changed=%s\n' "${PATH_COUNT}" "${IGNORED_CHANGED_COUNT}"
  printf 'reviewed_paths:\n'
  if [[ "${OUTPUT_MODE}" == --diff ]]; then
    sed 's/^/  /' "$(lease_file changed_paths)"
    git -C "${MAILCLI_ROOT}" diff --cached --stat "${BASELINE_HEAD}" --
    git -C "${MAILCLI_ROOT}" diff --cached "${BASELINE_HEAD}" --
  else
    LC_ALL=C awk 'NR <= 8 { print "  " substr($0, 1, 120) }' "$(lease_file changed_paths)"
    printf 'reviewed_paths_omitted=%s\nreviewed_paths_preview_max_bytes=120\n' "${OMITTED_COUNT}"
    LC_ALL=C git -C "${MAILCLI_ROOT}" -c core.quotePath=true diff --cached --stat --stat-width=80 \
      --stat-name-width=56 --stat-graph-width=20 --stat-count=8 "${BASELINE_HEAD}" --
  fi
  printf 'reviewed_patch_sha256=%s\n' "${PATCH_DIGEST}"
}

gate_lease() {
  local TOKEN="$1"
  shift
  local GATE_TIER=fast
  local GATE_ARGS=(--fast)
  if [[ "$#" -gt 0 ]]; then
    if [[ "$#" -eq 1 && ( "$1" == --fast || "$1" == --full ) ]]; then
      GATE_TIER="${1#--}"
    else
      [[ "$#" -ge 2 && "$1" == --checks ]] || fail 'Gate requires --fast, --full, or registered --checks paths'
      GATE_TIER=targeted
    fi
    GATE_ARGS=("$@")
  fi
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

  rm -f "$(lease_file gate_patch_sha256)" \
    "$(lease_file gate_index_tree)" "$(lease_file gate_harness)" "$(lease_file gate_tier)" "$(lease_file gate_shell_receipts)"
  local GATE_STATUS=0
  local HARNESS_DIR
  local INDEX_TREE
  local BASELINE_ORCHESTRATOR
  local GATE_HARNESS_MODE
  INDEX_TREE="$(git -C "${MAILCLI_ROOT}" write-tree)"
  BASELINE_ORCHESTRATOR="$(git -C "${MAILCLI_ROOT}" show "${BASELINE_HEAD}:scripts/tests/test.sh")"
  grep -Fxq '# MAILCLI_GATE_HARNESS=staged-v1' <<<"${BASELINE_ORCHESTRATOR}" ||
    fail 'Baseline gate harness marker is required'
  HARNESS_DIR="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-gate-harness.XXXXXX")"
  trap 'rm -rf -- "${HARNESS_DIR}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  git -C "${MAILCLI_ROOT}" archive "${BASELINE_HEAD}" scripts/utils |
    tar -x -C "${HARNESS_DIR}"
  "${HARNESS_DIR}/scripts/utils/run-staged-gate.sh" "${MAILCLI_ROOT}" \
    "${BASELINE_HEAD}" "${INDEX_TREE}" "${GATE_ARGS[@]}" |
    tee "${HARNESS_DIR}/gate-output" || GATE_STATUS=$?
  GATE_HARNESS_MODE="$(sed -n 's/^gate_harness=//p' "${HARNESS_DIR}/gate-output")"
  trap - INT TERM
  if [[ "${GATE_STATUS}" -ne 0 ]]; then
    printf '%s gate failed with status %s; commit proof was not recorded\n' "${GATE_TIER}" "${GATE_STATUS}" >&2
    rm -rf -- "${HARNESS_DIR}"
    trap - EXIT
    return "${GATE_STATUS}"
  fi

  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "HEAD changed while the full gate was running"
  verify_staged_scope
  [[ "$(allowed_state_digest)" == "${CURRENT_DIGEST}" ]] ||
    fail "Allowed file content changed while the full gate was running"
  [[ "$(staged_patch_digest)" == "${CURRENT_PATCH_DIGEST}" ]] ||
    fail "Staged patch changed while the full gate was running"
  [[ "$(git -C "${MAILCLI_ROOT}" write-tree)" == "${INDEX_TREE}" ]] ||
    fail "Index tree changed while the full gate was running"
  printf '%s\n' "${INDEX_TREE}" >"$(lease_file gate_index_tree)"
  printf '%s\n' "${GATE_HARNESS_MODE}" >"$(lease_file gate_harness)"
  printf '%s\n' "${GATE_TIER}" >"$(lease_file gate_tier)"
  if [[ -f "${HARNESS_DIR}/gate-output" ]]; then
    sed -n 's/^shell_test_receipt=//p' "${HARNESS_DIR}/gate-output" >"$(lease_file gate_shell_receipts)"
  fi
  rm -rf -- "${HARNESS_DIR}"
  trap - EXIT
  printf '%s\n' "${CURRENT_PATCH_DIGEST}" >"$(lease_file gate_patch_sha256)"
  if [[ "${GATE_TIER}" == full ]]; then printf 'full_gate=passed\n';
  else printf 'targeted_gate=passed\nfull_gate=deferred\n'; fi
  printf 'gated_patch_sha256=%s\n' "${CURRENT_PATCH_DIGEST}"
}

release_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  verify_ignored_asset_scope
  [[ -f "$(lease_file gate_patch_sha256)" ]] ||
    fail "No successful gate evidence exists for this lease"
  local TASK_ID
  local BASELINE_HEAD
  local CURRENT_HEAD
  local PARENT_HEAD
  local COMMIT_SUBJECT
  local SUBJECT_PREFIX
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
  SUBJECT_PREFIX="TASK ${TASK_ID//,/, }: "
  [[ "${COMMIT_SUBJECT}" == "${SUBJECT_PREFIX}"* &&
    "${COMMIT_SUBJECT}" != "${SUBJECT_PREFIX}" ]] ||
    fail "Commit subject must start with ${SUBJECT_PREFIX}and describe the change"
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
  printf 'task_commit_subject=%s\n' "${COMMIT_SUBJECT}"
  git -C "${MAILCLI_ROOT}" diff --stat "${BASELINE_HEAD}" "${CURRENT_HEAD}" --
  remove_lease_files
  printf 'write_lease=released\n'
  printf 'task_commit=%s\n' "${CURRENT_HEAD}"
}


# precommit is run by the local pre-commit hook: while a lease is active, a
# commit is allowed only for the exact staged patch that passed the gate.
precommit_check() {
  [[ -d "${LEASE_DIRECTORY}" ]] || return 0
  [[ -f "$(lease_file gate_patch_sha256)" ]] ||
    fail "Commit refused: the active write lease has no passed gate; run gate and check its exit code first"
  [[ "$(staged_patch_digest)" == "$(<"$(lease_file gate_patch_sha256)")" ]] ||
    fail "Commit refused: the staged patch differs from the patch that passed the gate"
}

abort_lease() {
  local TOKEN="$1"
  require_token "${TOKEN}"
  verify_ignored_asset_scope
  local BASELINE_HEAD
  BASELINE_HEAD="$(<"$(lease_file baseline_head)")"
  [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" ]] ||
    fail "Cannot abort a lease after HEAD changed"
  [[ -z "$(git -C "${MAILCLI_ROOT}" ls-files -v | grep -E '^[a-zS] ' || true)" ]] ||
    fail 'Cannot abort a lease with hidden index verification flags'
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
  extend)
    shift
    extend_lease "$@"
    ;;
  status)
    [[ "$#" -eq 1 ]] || fail "status accepts no additional arguments"
    status_lease
    ;;
  push-check)
    [[ "$#" -eq 1 ]] || fail 'push-check accepts no additional arguments'
    "${MAILCLI_ROOT}/scripts/utils/manage-full-proof.sh" check "${MAILCLI_ROOT}"
    ;;
  gate)
    [[ "$#" -ge 2 ]] || fail 'gate requires a lease token'
    shift
    gate_lease "$@"
    ;;
  review)
    [[ "$#" -eq 2 || ( "$#" -eq 3 && "$3" == --diff ) ]] || fail 'review requires a lease token and optional --diff'
    review_lease "$2" "${3:-}"
    ;;
  precommit)
    [[ "$#" -eq 1 ]] || fail 'precommit accepts no additional arguments'
    precommit_check
    ;;
  release)
    [[ "$#" -eq 2 ]] || fail "${COMMAND} requires exactly one lease token"
    release_lease "$2"
    ;;
  abort)
    [[ "$#" -eq 2 ]] || fail 'abort requires exactly one lease token'
    abort_lease "$2"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

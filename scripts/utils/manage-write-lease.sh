#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_ROOT="${MAILCLI_WRITE_ROOT:-${SCRIPT_ROOT}}"

usage() {
  printf '%s\n' \
    'Usage:' \
    '  manage-write-lease.sh acquire TASK_ID[,TASK_ID...] OWNER PATH [PATH...]' \
    '  manage-write-lease.sh start-worktree TASK_IDS OWNER EXPECTED_PRIMARY_HEAD DEST PATH [PATH...]' \
    '  manage-write-lease.sh integrate TOKEN OWNER [EXTRA_PATH...]' \
    '  manage-write-lease.sh register-current TOKEN  # TASK 511 bootstrap only' \
    '  manage-write-lease.sh status' \
    '  manage-write-lease.sh review TOKEN [--diff]' \
    '  manage-write-lease.sh gate TOKEN [--fast|--full|--checks REGISTERED_PATH... [--expect-baseline-failure PATH EXACT_FINAL_LINE]]' \
    '  manage-write-lease.sh push-check' \
    '  manage-write-lease.sh release TOKEN' \
    '  manage-write-lease.sh abort TOKEN [--recover-contract-drift]'
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
COMMON_DIRECTORY="$(git -C "${MAILCLI_ROOT}" rev-parse --path-format=absolute --git-common-dir)"
COMMON_DIRECTORY="$(cd "${COMMON_DIRECTORY}" && pwd -P)"
RESERVATIONS="${COMMON_DIRECTORY}/mailcli-write-reservations"
REGISTRY_MUTEX="${COMMON_DIRECTORY}/mailcli-write-reservations.lock"
WORKTREE_REFERENCE="${GIT_DIRECTORY}/mailcli-worktree-reference.json"
PRIMARY_ROOT="$(git -C "${MAILCLI_ROOT}" worktree list --porcelain | sed -n '1s/^worktree //p')"
PRIMARY_ROOT="$(cd "${PRIMARY_ROOT}" && pwd -P)"
TASK_ROOT="${MAILCLI_ROOT}"
INTEGRATION_TOKEN=''
RECOVER_CONTRACT_DRIFT=false

lease_file() {
  printf '%s/%s\n' "${LEASE_DIRECTORY}" "$1"
}

remove_lease_files() {
  local NAME
  for NAME in task owner_session token acquired_at baseline_head \
    allowed_paths allowed_fingerprints \
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
  verify_worker_context
  verify_reservation "${PROVIDED_TOKEN}" writing
}

verify_worker_context() {
  [[ "${MAILCLI_ROOT}" != "${PRIMARY_ROOT}" ]] || return 0
  [[ -f "${WORKTREE_REFERENCE}" && ! -L "${WORKTREE_REFERENCE}" ]] || fail 'Linked worktree requires source-bound owner instructions'
  jq -e --arg primary "${PRIMARY_ROOT}" --arg worker "${MAILCLI_ROOT}" '
    .schema == 1 and .primary == $primary and .worker == $worker and
    (.base | type == "string" and test("^[0-9a-f]{40}$")) and
    (.tasks | type == "string" and test("^[0-9]{3}(,[0-9]{3})*$")) and
    (.tree | type == "string" and test("^[0-9a-f]{40}$")) and
    (.paths | type == "array" and length > 0 and all(.[]; type == "string" and length > 0)) and
    (.contracts | type == "array" and length > 1) and
    ([.contracts[].path] | length == (unique | length)) and
    ([.contracts[] | select(.path == "AGENTS.local.md")] | length == 1) and
    ([.contracts[] | select(.path != "AGENTS.local.md") | .path[11:14]] | sort) == (.tasks | split(",") | sort) and
    all(.contracts[]; (.path == "AGENTS.local.md" or (.path | test("^docs/tasks/[0-9]{3}-[a-z0-9-]+\\.md$"))) and
      (.sha256 | type == "string" and test("^[0-9a-f]{64}$")))
  ' "${WORKTREE_REFERENCE}" >/dev/null || fail 'Invalid worktree reference'
  TASK_ROOT="${PRIMARY_ROOT}"
  # Relinquishing unchanged work needs the original ownership identity, not
  # restoration of private instructions that the owner has since updated.
  if [[ "${COMMAND}" == abort && "${RECOVER_CONTRACT_DRIFT}" == true ]]; then
    jq -e --arg base "$(<"$(lease_file baseline_head)")" \
      --arg tree "$(git -C "${MAILCLI_ROOT}" rev-parse "$(<"$(lease_file baseline_head)")^{tree}")" \
      --arg tasks "$(<"$(lease_file task)")" --rawfile paths "$(lease_file allowed_paths)" '
      .base == $base and .tree == $tree and .tasks == $tasks and
      (.paths | sort) == ($paths | split("\n") | map(select(length > 0)) | sort)
    ' "${WORKTREE_REFERENCE}" >/dev/null || fail 'Worker recovery reference differs from the original lease'
    return 0
  fi
  local PATH_NAME EXPECTED_DIGEST ROOT
  while IFS=$'\t' read -r PATH_NAME EXPECTED_DIGEST; do
    for ROOT in "${PRIMARY_ROOT}" "${MAILCLI_ROOT}"; do
      [[ -f "${ROOT}/${PATH_NAME}" && ! -L "${ROOT}/${PATH_NAME}" ]] || fail 'Worktree contract file is missing or unsafe'
      [[ "$(shasum -a 256 "${ROOT}/${PATH_NAME}" | awk '{print $1}')" == "${EXPECTED_DIGEST}" ]] || fail 'Worktree task contract or owner instructions changed'
    done
  done < <(jq -r '.contracts[] | [.path,.sha256] | @tsv' "${WORKTREE_REFERENCE}")
}

verify_reservation() {
  local TOKEN="$1" STATE="$2" RECORD="${RESERVATIONS}/$1.json"
  [[ "${TOKEN}" =~ ^[0-9a-f]{64}$ && -f "${RECORD}" && ! -L "${RECORD}" ]] || fail 'Shared reservation is missing or unsafe'
  jq -e --arg token "${TOKEN}" --arg root "${MAILCLI_ROOT}" --arg state "${STATE}" \
    --arg tasks "$(<"$(lease_file task)")" --arg owner "$(<"$(lease_file owner_session)")" \
    --arg base "$(<"$(lease_file baseline_head)")" --rawfile paths "$(lease_file allowed_paths)" '
    .schema == 1 and .token == $token and .root == $root and .state == $state and
    .tasks == $tasks and .owner == $owner and .base == $base and
    .paths == ($paths | split("\n") | map(select(length > 0)))
  ' "${RECORD}" >/dev/null || fail 'Shared reservation differs from the exact local lease'
}

reserve_lease() (
  local TOKEN="$1" REPLACE_TOKEN="${2:-}" RECORD OTHER_TASK TASK_ID LEFT RIGHT
  [[ ! -L "${RESERVATIONS}" ]] || fail 'Shared reservation directory must not be a symlink'
  mkdir "${REGISTRY_MUTEX}" 2>/dev/null || fail 'Shared reservation registry is busy; never steal its mutex'
  trap 'rmdir "${REGISTRY_MUTEX}"' EXIT
  umask 077
  mkdir -p "${RESERVATIONS}"
  shopt -s nullglob dotglob
  local CASE_INSENSITIVE=false
  [[ "$(git -C "${MAILCLI_ROOT}" config --get core.ignorecase || true)" != true ]] || CASE_INSENSITIVE=true
  for RECORD in "${RESERVATIONS}/"*; do
    [[ -f "${RECORD}" && ! -L "${RECORD}" && "${RECORD##*/}" =~ ^[0-9a-f]{64}\.json$ ]] || fail 'Incomplete shared reservation requires owner recovery'
    jq -e --arg token "${RECORD##*/}" '.schema == 1 and (.token + ".json") == $token and (.token | test("^[0-9a-f]{64}$")) and
      (.tasks | test("^[0-9]{3}(,[0-9]{3})*$")) and
      (.root | type == "string") and (.state | IN("writing","ready-for-integration")) and
      (.paths | type == "array" and length > 0 and all(.[]; type == "string" and length > 0))' \
      "${RECORD}" >/dev/null || fail 'Incomplete shared reservation requires owner recovery'
    [[ "${RECORD}" != "${RESERVATIONS}/${REPLACE_TOKEN}.json" ]] || continue
    OTHER_TASK="$(jq -r '.tasks' "${RECORD}")"
    while IFS= read -r TASK_ID; do
      [[ ",${OTHER_TASK}," != *",${TASK_ID},"* ]] || fail "TASK ${TASK_ID} is already reserved"
    done < <(tr ',' '\n' <"$(lease_file task)")
    while IFS= read -r LEFT; do
      while IFS= read -r RIGHT; do
        if [[ "${CASE_INSENSITIVE}" == true ]]; then
          LEFT="$(LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"${LEFT}")"
          RIGHT="$(LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"${RIGHT}")"
        fi
        [[ "${LEFT}" != "${RIGHT}" && "${LEFT}" != "${RIGHT}/"* && "${RIGHT}" != "${LEFT}/"* ]] || fail "Path overlaps another reservation: ${LEFT}"
      done < <(jq -r '.paths[]' "${RECORD}")
    done <"$(lease_file allowed_paths)"
  done
  local CANDIDATE="${RESERVATIONS}/.candidate-${TOKEN}"
  [[ ! -e "${CANDIDATE}" && ! -L "${CANDIDATE}" ]] || fail 'Reservation candidate already exists'
  trap 'rm -f "${CANDIDATE}"; rmdir "${REGISTRY_MUTEX}"' EXIT
  jq -n --arg token "${TOKEN}" --arg root "${MAILCLI_ROOT}" --arg owner "$(<"$(lease_file owner_session)")" \
    --arg tasks "$(<"$(lease_file task)")" --arg base "$(<"$(lease_file baseline_head)")" \
    --rawfile paths "$(lease_file allowed_paths)" '{schema:1,token:$token,root:$root,owner:$owner,tasks:$tasks,base:$base,
      state:"writing",paths:($paths | split("\n") | map(select(length > 0)))}' >"${CANDIDATE}"
  if [[ -n "${REPLACE_TOKEN}" ]]; then
    [[ "${REPLACE_TOKEN}" == "${TOKEN}" ]] || fail 'Integration token mismatch'
    [[ "$(jq -r '.state' "${RESERVATIONS}/${TOKEN}.json")" == ready-for-integration ]] || fail 'Worker has no completed integration handoff'
    [[ ! -e "${GIT_DIRECTORY}/mailcli-integration-origin-${TOKEN}.json" && ! -L "${GIT_DIRECTORY}/mailcli-integration-origin-${TOKEN}.json" ]] || fail 'Integration origin already exists; recover it without overwriting'
    mv "${RESERVATIONS}/${TOKEN}.json" "${GIT_DIRECTORY}/mailcli-integration-origin-${TOKEN}.json"
  else
    [[ ! -e "${RESERVATIONS}/${TOKEN}.json" ]] || fail 'Reservation token already exists'
  fi
  mv "${CANDIDATE}" "${RESERVATIONS}/${TOKEN}.json"
)

close_reservation() (
  local TOKEN="$1" CURRENT_HEAD="${2:-}" RECORD="${RESERVATIONS}/$1.json"
  mkdir "${REGISTRY_MUTEX}" 2>/dev/null || fail 'Shared reservation registry is busy; ownership is retained'
  trap 'rmdir "${REGISTRY_MUTEX}"' EXIT
  verify_reservation "${TOKEN}" writing
  umask 077
  if [[ -n "${CURRENT_HEAD}" && "${MAILCLI_ROOT}" != "${PRIMARY_ROOT}" ]]; then
    local CANDIDATE="${RESERVATIONS}/.candidate-${TOKEN}"
    [[ ! -e "${CANDIDATE}" && ! -L "${CANDIDATE}" ]] || fail 'Reservation candidate already exists'
    [[ ! -e "${GIT_DIRECTORY}/mailcli-writing-origin-${TOKEN}.json" && ! -L "${GIT_DIRECTORY}/mailcli-writing-origin-${TOKEN}.json" ]] || fail 'Worker origin already exists; recover it without overwriting'
    trap 'rm -f "${CANDIDATE}"; rmdir "${REGISTRY_MUTEX}"' EXIT
    jq --arg head "${CURRENT_HEAD}" '.state = "ready-for-integration" | .commit = $head' "${RECORD}" >"${CANDIDATE}"
    mv "${RECORD}" "${GIT_DIRECTORY}/mailcli-writing-origin-${TOKEN}.json"
    mv "${CANDIDATE}" "${RECORD}"
    printf 'integration_token=%s\nintegration_commit=%s\n' "${TOKEN}" "${CURRENT_HEAD}"
  else
    rm "${RECORD}"
  fi
)

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
        docs/tasks.md | docs/tasks | docs/tasks/*) continue ;;
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
      case "${RELATIVE_PATH}" in docs/tasks | docs/tasks/*) continue ;; esac
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

register_current() {
  local TOKEN="$1"
  [[ "${MAILCLI_ROOT}" == "${PRIMARY_ROOT}" ]] || fail 'Bootstrap requires the primary worktree'
  require_lease
  [[ "${TOKEN}" == "$(<"$(lease_file token)")" && "$(<"$(lease_file task)")" == 511 &&
    "$(<"$(lease_file baseline_head)")" == "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" ]] || fail 'Only the existing TASK 511 lease may bootstrap shared ownership'
  [[ ! -e "${RESERVATIONS}/${TOKEN}.json" && ! -L "${RESERVATIONS}/${TOKEN}.json" ]] || fail 'Bootstrap reservation already exists'
  reserve_lease "${TOKEN}"
  verify_reservation "${TOKEN}" writing
  printf 'shared_reservation=registered\n'
}

start_worktree() {
  [[ "$#" -ge 5 ]] || fail 'start-worktree requires TASK IDs, owner, primary HEAD, destination and paths'
  [[ "${MAILCLI_ROOT}" == "${PRIMARY_ROOT}" ]] || fail 'Start workers from the primary worktree'
  local TASK_IDS OWNER EXPECTED_HEAD DEST PATH_NAME TASK_ID ENTRY WORKER_GIT CONTRACTS OUTPUT TOKEN
  TASK_IDS="$(normalize_task_ids "$1")"; OWNER="$2"; EXPECTED_HEAD="$3"; DEST="$4"
  shift 4
  [[ "${EXPECTED_HEAD}" =~ ^[0-9a-f]{40}$ && "$(git -C "${PRIMARY_ROOT}" rev-parse HEAD)" == "${EXPECTED_HEAD}" ]] || fail 'Explicit primary HEAD is stale or invalid'
  [[ -n "${OWNER}" && "${OWNER}" != *$'\n'* ]] || fail 'OWNER must be one line'
  [[ "${DEST}" == /* && ! -e "${DEST}" && ! -L "${DEST}" && -d "$(dirname "${DEST}")" ]] || fail 'Worker destination must be an absent absolute path with an existing parent'
  DEST="$(cd "$(dirname "${DEST}")" && pwd -P)/$(basename "${DEST}")"
  [[ -z "$(git -C "${PRIMARY_ROOT}" status --porcelain=v1 --untracked-files=all)" ]] || fail 'Primary worktree must be clean before starting a worker'
  validate_task_group "${TASK_IDS}" strict
  for PATH_NAME in "$@"; do
    validate_allowed_path "${PATH_NAME}"
    case "${PATH_NAME}" in AGENTS.local.md|docs/tasks.md|docs/tasks|docs/tasks/*) fail 'Worker assignments cannot write the private task control plane' ;; esac
  done
  local -a CONTRACT_PATHS=(AGENTS.local.md)
  while IFS= read -r TASK_ID; do
    ENTRY="$(grep -E "^- \\[[ ~]\\] ${TASK_ID} .+ -> tasks/${TASK_ID}-[a-z0-9-]+\\.md$" "${PRIMARY_ROOT}/docs/tasks.md")"
    CONTRACT_PATHS+=("docs/${ENTRY##* -> }")
  done < <(tr ',' '\n' <<<"${TASK_IDS}")
  CONTRACTS='[]'
  for PATH_NAME in "${CONTRACT_PATHS[@]}"; do
    validate_allowed_path "${PATH_NAME}"
    [[ -f "${PRIMARY_ROOT}/${PATH_NAME}" && ! -L "${PRIMARY_ROOT}/${PATH_NAME}" ]] || fail "Missing regular worker contract: ${PATH_NAME}"
    git -C "${PRIMARY_ROOT}" check-ignore -q -- "${PATH_NAME}" || fail "Worker contract must remain private: ${PATH_NAME}"
    CONTRACTS="$(jq --arg path "${PATH_NAME}" --arg sha "$(shasum -a 256 "${PRIMARY_ROOT}/${PATH_NAME}" | awk '{print $1}')" '. + [{path:$path,sha256:$sha}]' <<<"${CONTRACTS}")"
  done
  git -C "${PRIMARY_ROOT}" worktree add --detach --lock --reason "MailCLI TASK ${TASK_IDS}: ${OWNER}" "${DEST}" "${EXPECTED_HEAD}"
  [[ "$(git -C "${PRIMARY_ROOT}" rev-parse HEAD)" == "${EXPECTED_HEAD}" && "$(git -C "${DEST}" rev-parse HEAD)" == "${EXPECTED_HEAD}" ]] || fail 'Primary changed while starting the worker; preserve the worktree for recovery'
  for PATH_NAME in "${CONTRACT_PATHS[@]}"; do
    [[ ! -e "${DEST}/${PATH_NAME}" && ! -L "${DEST}/${PATH_NAME}" ]] || fail 'Worker contract destination already exists'
    (MAILCLI_ROOT="${DEST}"; validate_allowed_path "${PATH_NAME}")
    mkdir -p "$(dirname "${DEST}/${PATH_NAME}")"
    cp "${PRIMARY_ROOT}/${PATH_NAME}" "${DEST}/${PATH_NAME}"
    cmp -s "${PRIMARY_ROOT}/${PATH_NAME}" "${DEST}/${PATH_NAME}" || fail 'Worker contract copy differs'
    chmod 400 "${DEST}/${PATH_NAME}"
  done
  WORKER_GIT="$(git -C "${DEST}" rev-parse --absolute-git-dir)"
  [[ ! -e "${WORKER_GIT}/mailcli-worktree-reference.json" && ! -L "${WORKER_GIT}/mailcli-worktree-reference.json" ]] || fail 'Worker reference already exists'
  (umask 077; printf '%s\n' "$@" | jq -Rn --arg primary "${PRIMARY_ROOT}" --arg worker "${DEST}" --arg base "${EXPECTED_HEAD}" --arg tree "$(git -C "${DEST}" rev-parse HEAD^{tree})" --arg tasks "${TASK_IDS}" --argjson contracts "${CONTRACTS}" '{schema:1,primary:$primary,worker:$worker,base:$base,tree:$tree,tasks:$tasks,contracts:$contracts,paths:[inputs]}' >"${WORKER_GIT}/mailcli-worktree-reference.json")
  OUTPUT="$(MAILCLI_WRITE_ROOT="${DEST}" "${SCRIPT_ROOT}/scripts/utils/manage-write-lease.sh" acquire "${TASK_IDS}" "${OWNER}" "$@")" || fail 'Worker was created but not authorized; preserve it for recovery'
  TOKEN="$(sed -n 's/^write_lease_token=//p' <<<"${OUTPUT}")"
  if [[ "$(git -C "${PRIMARY_ROOT}" rev-parse HEAD)" != "${EXPECTED_HEAD}" ]]; then
    MAILCLI_WRITE_ROOT="${DEST}" "${SCRIPT_ROOT}/scripts/utils/manage-write-lease.sh" abort "${TOKEN}"
    fail 'Primary changed before worker authorization; restart from the current primary HEAD'
  fi
  printf 'worker_root=%s\n%s\n' "${DEST}" "${OUTPUT}"
}

integrate_worktree() {
  [[ "$#" -ge 2 ]] || fail 'integrate requires a worker token and primary owner'
  [[ "${MAILCLI_ROOT}" == "${PRIMARY_ROOT}" && ! -e "${LEASE_DIRECTORY}" ]] || fail 'Integration requires the primary worktree without an active lease'
  local TOKEN="$1" OWNER="$2" RECORD COMMIT TASK_IDS
  shift 2
  [[ "${TOKEN}" =~ ^[0-9a-f]{64}$ ]] || fail 'Invalid integration token'
  RECORD="${RESERVATIONS}/${TOKEN}.json"
  [[ -f "${RECORD}" && ! -L "${RECORD}" ]] || fail 'Worker reservation is missing or unsafe'
  jq -e --arg token "${TOKEN}" '.schema == 1 and .token == $token and .state == "ready-for-integration" and (.commit | test("^[0-9a-f]{40}$")) and (.tasks | test("^[0-9]{3}(,[0-9]{3})*$")) and (.paths | type == "array" and length > 0 and all(.[]; type == "string"))' "${RECORD}" >/dev/null || fail 'Worker handoff is incomplete'
  COMMIT="$(jq -r '.commit' "${RECORD}")"
  git -C "${PRIMARY_ROOT}" cat-file -e "${COMMIT}^{commit}" || fail 'Worker commit is unavailable'
  local WORKER_ROOT WORKER_GIT ORIGIN
  WORKER_ROOT="$(jq -r '.root' "${RECORD}")"
  [[ -d "${WORKER_ROOT}" && "$(git -C "${WORKER_ROOT}" rev-parse HEAD)" == "${COMMIT}" &&
    "$(git -C "${WORKER_ROOT}" rev-parse --path-format=absolute --git-common-dir)" == "${COMMON_DIRECTORY}" ]] || fail 'Worker handoff source changed or belongs to another repository'
  WORKER_GIT="$(git -C "${WORKER_ROOT}" rev-parse --absolute-git-dir)"
  ORIGIN="${WORKER_GIT}/mailcli-writing-origin-${TOKEN}.json"
  [[ -f "${ORIGIN}" && ! -L "${ORIGIN}" ]] || fail 'Worker writing origin is unavailable'
  jq -e --slurpfile origin "${ORIGIN}" 'del(.commit) | .state = "writing" | . == $origin[0]' "${RECORD}" >/dev/null || fail 'Worker handoff differs from its released ownership'
  TASK_IDS="$(jq -r '.tasks' "${RECORD}")"
  validate_task_group "${TASK_IDS}" strict
  local -a PATHS=()
  local PATH_NAME
  while IFS= read -r PATH_NAME; do PATHS+=("${PATH_NAME}"); done < <(jq -r '.paths[]' "${RECORD}")
  INTEGRATION_TOKEN="${TOKEN}"
  acquire_lease "${TASK_IDS}" "${OWNER}" "${PATHS[@]}" "$@"
  printf 'integration_commit=%s\nintegration_requires=fresh review and gate after cherry-pick --no-commit\n' "${COMMIT}"
}

validate_task_group() {
  local TASK_IDS="$1"
  local TASK_ID
  local MATCH_COUNT
  local ENTRY_COUNT
  local BOARD_ENTRY
  local DETAIL_PATH
  local BOARD_PATH="${TASK_ROOT}/docs/tasks.md"
  [[ "${TASK_IDS}" == *,* || "${TASK_ROOT}" != "${MAILCLI_ROOT}" || "${2:-}" == strict ]] || return 0
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
    DETAIL_PATH="${TASK_ROOT}/docs/${BOARD_ENTRY##* -> }"
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
  verify_worker_context
  if [[ "${MAILCLI_ROOT}" != "${PRIMARY_ROOT}" ]]; then
    [[ "${TASK_ID}" == "$(jq -r '.tasks' "${WORKTREE_REFERENCE}")" &&
      "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" == "$(jq -r '.base' "${WORKTREE_REFERENCE}")" &&
      "$(git -C "${PRIMARY_ROOT}" rev-parse HEAD)" == "$(jq -r '.base' "${WORKTREE_REFERENCE}")" ]] || fail 'Worker TASK set or primary integration base is stale'
    [[ "$(printf '%s\n' "$@" | LC_ALL=C sort -u)" == "$(jq -r '.paths[]' "${WORKTREE_REFERENCE}" | LC_ALL=C sort -u)" ]] || fail 'Worker paths differ from its source-bound assignment'
  fi
  for RELATIVE_PATH in "$@"; do
    validate_allowed_path "${RELATIVE_PATH}"
    if [[ "${MAILCLI_ROOT}" != "${PRIMARY_ROOT}" ]]; then
      case "${RELATIVE_PATH}" in AGENTS.local.md|docs/tasks.md|docs/tasks|docs/tasks/*) fail 'Workers cannot write the private task control plane' ;; esac
    fi
  done

  if ! mkdir "${LEASE_DIRECTORY}" 2>/dev/null; then
    printf 'Another writer owns this worktree:\n' >&2
    status_lease >&2
    exit 1
  fi
  chmod 700 "${LEASE_DIRECTORY}"
  local ACQUIRE_COMPLETE=false
  trap 'if [[ "${ACQUIRE_COMPLETE:-false}" != true && ! -f "${RESERVATIONS}/${TOKEN:-none}.json" ]]; then remove_lease_files; fi' EXIT
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
  TOKEN="${INTEGRATION_TOKEN:-$(od -v -An -N32 -tx1 /dev/urandom | tr -d '[:space:]')}"
  [[ "${TOKEN}" =~ ^[0-9a-f]{64}$ ]] || fail 'Could not read 32 random token bytes'
  printf '%s\n' "${TOKEN}" >"$(lease_file token)"
  reserve_lease "${TOKEN}" "${INTEGRATION_TOKEN:-}"
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
    "${MAILCLI_ROOT}/scripts/utils/run-fast-gate.sh" "${MAILCLI_ROOT}" "${BASELINE_HEAD}" "${INDEX_TREE}" --lint-only "$(lease_file lint_identity)" "$(<"$(lease_file task)")"
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
    "${BASELINE_HEAD}" "${INDEX_TREE}" "$(<"$(lease_file task)")" "${GATE_ARGS[@]}" |
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
  close_reservation "${TOKEN}" "${CURRENT_HEAD}"
  remove_lease_files
  printf 'write_lease=released\n'
  printf 'task_commit=%s\n' "${CURRENT_HEAD}"
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
  close_reservation "${TOKEN}"
  remove_lease_files
  printf 'write_lease=aborted\n'
}

COMMAND="${1:-}"
case "${COMMAND}" in
  start-worktree)
    shift
    start_worktree "$@"
    ;;
  integrate)
    shift
    integrate_worktree "$@"
    ;;
  register-current)
    [[ "$#" -eq 2 ]] || fail 'register-current requires exactly one lease token'
    register_current "$2"
    ;;
  acquire)
    shift
    acquire_lease "$@"
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
  release)
    [[ "$#" -eq 2 ]] || fail "${COMMAND} requires exactly one lease token"
    release_lease "$2"
    ;;
  abort)
    [[ "$#" -eq 2 || ( "$#" -eq 3 && "$3" == --recover-contract-drift ) ]] ||
      fail 'abort requires a lease token and optional --recover-contract-drift'
    [[ "$#" -ne 3 ]] || RECOVER_CONTRACT_DRIFT=true
    abort_lease "$2"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

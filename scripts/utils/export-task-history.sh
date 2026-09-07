#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEFAULT_BACKUP_ROOT="${HOME}/Library/Application Support/MailCLI/task-history-backups"
MANIFEST_NAME="MANIFEST.sha256"
STAGING_DIRECTORY=""
TEMPORARY_DIRECTORY=""
TEMPORARY_COUNTER=0
NEW_TEMPORARY_PATH=""

usage() {
  printf '%s\n' \
    'Usage:' \
    '  export-task-history.sh create [ABSOLUTE_DESTINATION_ROOT]' \
    '  export-task-history.sh verify ABSOLUTE_SNAPSHOT_DIRECTORY'
}

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

cleanup() {
  if [[ -n "${TEMPORARY_DIRECTORY}" &&
    "$(basename "${TEMPORARY_DIRECTORY}")" == mailcli-task-history-export.* &&
    -d "${TEMPORARY_DIRECTORY}" ]]; then
    rm -rf -- "${TEMPORARY_DIRECTORY}"
  fi
  if [[ -n "${STAGING_DIRECTORY}" &&
    "$(basename "${STAGING_DIRECTORY}")" == .mailcli-task-history-*.incomplete &&
    -d "${STAGING_DIRECTORY}" ]]; then
    rm -rf -- "${STAGING_DIRECTORY}"
  fi
}
trap cleanup EXIT

new_temporary_path() {
  local PREFIX="$1"
  if [[ -z "${TEMPORARY_DIRECTORY}" ]]; then
    TEMPORARY_DIRECTORY="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-task-history-export.XXXXXX")"
    chmod 700 "${TEMPORARY_DIRECTORY}"
  fi
  TEMPORARY_COUNTER=$((TEMPORARY_COUNTER + 1))
  NEW_TEMPORARY_PATH="${TEMPORARY_DIRECTORY}/${TEMPORARY_COUNTER}-${PREFIX}"
}

path_mode() {
  local PATH_TO_CHECK="$1"
  if [[ "$(uname -s)" == "Darwin" ]]; then
    stat -f '%Lp' "${PATH_TO_CHECK}"
    return
  fi
  stat -c '%a' "${PATH_TO_CHECK}"
}

require_mode() {
  local PATH_TO_CHECK="$1"
  local EXPECTED_MODE="$2"
  local ACTUAL_MODE
  ACTUAL_MODE="$(path_mode "${PATH_TO_CHECK}")"
  [[ "${ACTUAL_MODE}" == "${EXPECTED_MODE}" ]] ||
    fail "Unsafe mode ${ACTUAL_MODE} for ${PATH_TO_CHECK}; expected ${EXPECTED_MODE}"
}

path_is_at_or_below() {
  local CANDIDATE_PATH="$1"
  local PARENT_PATH="$2"
  [[ "${CANDIDATE_PATH}" == "${PARENT_PATH}" || "${CANDIDATE_PATH}" == "${PARENT_PATH}/"* ]]
}

collect_task_paths() {
  local OUTPUT_PATH="$1"
  {
    printf '%s\n' 'docs/tasks.md'
    find "${MAILCLI_ROOT}/docs/tasks" -type f -name '*.md' -print |
      sed "s#^${MAILCLI_ROOT}/##" |
      LC_ALL=C sort
  } >"${OUTPUT_PATH}"
}

validate_task_sources() {
  local PATH_LIST="$1"
  [[ -f "${MAILCLI_ROOT}/docs/tasks.md" && ! -L "${MAILCLI_ROOT}/docs/tasks.md" ]] ||
    fail 'Task board is missing or is not a regular file'
  [[ -d "${MAILCLI_ROOT}/docs/tasks" && ! -L "${MAILCLI_ROOT}/docs/tasks" ]] ||
    fail 'Task detail directory is missing or is not a real directory'

  local UNSUPPORTED_PATH
  UNSUPPORTED_PATH="$(find "${MAILCLI_ROOT}/docs/tasks" ! -type d ! -type f -print -quit)"
  [[ -z "${UNSUPPORTED_PATH}" ]] ||
    fail "Task history contains a symlink or unsupported path: ${UNSUPPORTED_PATH}"
  UNSUPPORTED_PATH="$(find "${MAILCLI_ROOT}/docs/tasks" -type f ! -name '*.md' -print -quit)"
  [[ -z "${UNSUPPORTED_PATH}" ]] ||
    fail "Task history contains a non-Markdown file: ${UNSUPPORTED_PATH}"

  local RELATIVE_PATH
  local PATH_COUNT=0
  while IFS= read -r RELATIVE_PATH; do
    [[ "${RELATIVE_PATH}" == 'docs/tasks.md' ||
      "${RELATIVE_PATH}" =~ ^docs/tasks/(done/)?[0-9]{3}-[a-z0-9-]+\.md$ ]] ||
      fail "Task history path violates the canonical naming contract: ${RELATIVE_PATH}"
    [[ -f "${MAILCLI_ROOT}/${RELATIVE_PATH}" && ! -L "${MAILCLI_ROOT}/${RELATIVE_PATH}" ]] ||
      fail "Task history source is not a regular file: ${RELATIVE_PATH}"
    PATH_COUNT=$((PATH_COUNT + 1))
  done <"${PATH_LIST}"
  [[ "${PATH_COUNT}" -gt 1 ]] || fail 'Task history contains no detail files'
}

write_manifest() {
  local SOURCE_ROOT="$1"
  local PATH_LIST="$2"
  local OUTPUT_PATH="$3"
  local RELATIVE_PATH
  local DIGEST
  [[ ! -e "${OUTPUT_PATH}" ]] || fail "Manifest destination already exists: ${OUTPUT_PATH}"
  while IFS= read -r RELATIVE_PATH; do
    DIGEST="$(shasum -a 256 "${SOURCE_ROOT}/${RELATIVE_PATH}" | awk '{print $1}')"
    printf '%s\t%s\n' "${DIGEST}" "${RELATIVE_PATH}"
  done <"${PATH_LIST}" >"${OUTPUT_PATH}"
  chmod 600 "${OUTPUT_PATH}"
}

verify_snapshot() {
  local SNAPSHOT_DIRECTORY="$1"
  [[ "${SNAPSHOT_DIRECTORY}" == /* ]] || fail 'Snapshot directory must be absolute'
  [[ -d "${SNAPSHOT_DIRECTORY}" && ! -L "${SNAPSHOT_DIRECTORY}" ]] ||
    fail "Snapshot directory is missing or unsafe: ${SNAPSHOT_DIRECTORY}"
  SNAPSHOT_DIRECTORY="$(cd "${SNAPSHOT_DIRECTORY}" && pwd -P)"
  require_mode "${SNAPSHOT_DIRECTORY}" 700

  local MANIFEST_PATH="${SNAPSHOT_DIRECTORY}/${MANIFEST_NAME}"
  [[ -f "${MANIFEST_PATH}" && ! -L "${MANIFEST_PATH}" ]] ||
    fail "Snapshot manifest is missing or unsafe: ${MANIFEST_PATH}"

  local UNSUPPORTED_PATH
  UNSUPPORTED_PATH="$(find "${SNAPSHOT_DIRECTORY}" ! -type d ! -type f -print -quit)"
  [[ -z "${UNSUPPORTED_PATH}" ]] ||
    fail "Snapshot contains a symlink or unsupported path: ${UNSUPPORTED_PATH}"

  local DIRECTORY_PATH
  while IFS= read -r DIRECTORY_PATH; do
    require_mode "${DIRECTORY_PATH}" 700
  done < <(find "${SNAPSHOT_DIRECTORY}" -type d -print)
  local FILE_PATH
  while IFS= read -r FILE_PATH; do
    require_mode "${FILE_PATH}" 600
  done < <(find "${SNAPSHOT_DIRECTORY}" -type f -print)

  local EXPECTED_PATHS
  local ACTUAL_PATHS
  new_temporary_path expected
  EXPECTED_PATHS="${NEW_TEMPORARY_PATH}"
  new_temporary_path actual
  ACTUAL_PATHS="${NEW_TEMPORARY_PATH}"
  cut -f 2- "${MANIFEST_PATH}" >"${EXPECTED_PATHS}"
  (
    cd "${SNAPSHOT_DIRECTORY}"
    find . -type f ! -path "./${MANIFEST_NAME}" -print |
      sed 's#^\./##' |
      LC_ALL=C sort
  ) >"${ACTUAL_PATHS}"
  cmp -s "${EXPECTED_PATHS}" "${ACTUAL_PATHS}" || fail 'Snapshot file set differs from its manifest'

  local EXPECTED_DIGEST
  local RELATIVE_PATH
  local ACTUAL_DIGEST
  local VERIFIED_COUNT=0
  while IFS=$'\t' read -r EXPECTED_DIGEST RELATIVE_PATH; do
    [[ "${EXPECTED_DIGEST}" =~ ^[0-9a-f]{64}$ && -n "${RELATIVE_PATH}" ]] ||
      fail 'Snapshot manifest contains a malformed entry'
    [[ "${RELATIVE_PATH}" == 'docs/tasks.md' ||
      "${RELATIVE_PATH}" =~ ^docs/tasks/(done/)?[0-9]{3}-[a-z0-9-]+\.md$ ]] ||
      fail "Snapshot manifest contains an unsafe path: ${RELATIVE_PATH}"
    ACTUAL_DIGEST="$(shasum -a 256 "${SNAPSHOT_DIRECTORY}/${RELATIVE_PATH}" | awk '{print $1}')"
    [[ "${ACTUAL_DIGEST}" == "${EXPECTED_DIGEST}" ]] ||
      fail "Snapshot digest mismatch: ${RELATIVE_PATH}"
    VERIFIED_COUNT=$((VERIFIED_COUNT + 1))
  done <"${MANIFEST_PATH}"
  [[ "${VERIFIED_COUNT}" -gt 1 ]] || fail 'Snapshot manifest contains no task detail files'
  printf 'snapshot_verified=%s\nverified_files=%d\n' "${SNAPSHOT_DIRECTORY}" "${VERIFIED_COUNT}"
}

create_snapshot() {
  local DESTINATION_ROOT="${1:-${DEFAULT_BACKUP_ROOT}}"
  [[ "${DESTINATION_ROOT}" == /* ]] || fail 'Destination root must be absolute'
  case "${DESTINATION_ROOT}" in
    */. | */./* | */.. | */../*)
      fail 'Destination root must not contain ambiguous path components'
      ;;
  esac
  [[ "${DESTINATION_ROOT}" != / && "${DESTINATION_ROOT}" != "${HOME}" ]] ||
    fail 'Destination root is too broad or overlaps the repository'
  ! path_is_at_or_below "${DESTINATION_ROOT}" "${MAILCLI_ROOT}" ||
    fail 'Destination root must stay outside the repository'
  [[ ! -L "${DESTINATION_ROOT}" ]] || fail 'Destination root must not be a symlink'

  local EXISTING_ANCESTOR="${DESTINATION_ROOT}"
  local PARENT_PATH
  while [[ ! -e "${EXISTING_ANCESTOR}" ]]; do
    PARENT_PATH="$(dirname "${EXISTING_ANCESTOR}")"
    [[ "${PARENT_PATH}" != "${EXISTING_ANCESTOR}" ]] || fail 'Cannot resolve destination root'
    EXISTING_ANCESTOR="${PARENT_PATH}"
  done
  [[ -d "${EXISTING_ANCESTOR}" ]] ||
    fail "Destination ancestor is not a directory: ${EXISTING_ANCESTOR}"
  EXISTING_ANCESTOR="$(cd "${EXISTING_ANCESTOR}" && pwd -P)"
  ! path_is_at_or_below "${EXISTING_ANCESTOR}" "${MAILCLI_ROOT}" ||
    fail 'Destination root resolves inside the repository'

  umask 077
  mkdir -p "${DESTINATION_ROOT}"
  [[ -d "${DESTINATION_ROOT}" ]] || fail "Destination root is not a directory: ${DESTINATION_ROOT}"
  DESTINATION_ROOT="$(cd "${DESTINATION_ROOT}" && pwd -P)"
  ! path_is_at_or_below "${DESTINATION_ROOT}" "${MAILCLI_ROOT}" ||
    fail 'Resolved destination root must stay outside the repository'
  require_mode "${DESTINATION_ROOT}" 700

  local PATH_LIST
  local PATH_LIST_AFTER
  local SOURCE_MANIFEST_BEFORE
  local SOURCE_MANIFEST_AFTER
  new_temporary_path paths
  PATH_LIST="${NEW_TEMPORARY_PATH}"
  new_temporary_path paths-after
  PATH_LIST_AFTER="${NEW_TEMPORARY_PATH}"
  new_temporary_path source-before
  SOURCE_MANIFEST_BEFORE="${NEW_TEMPORARY_PATH}"
  new_temporary_path source-after
  SOURCE_MANIFEST_AFTER="${NEW_TEMPORARY_PATH}"
  collect_task_paths "${PATH_LIST}"
  validate_task_sources "${PATH_LIST}"
  write_manifest "${MAILCLI_ROOT}" "${PATH_LIST}" "${SOURCE_MANIFEST_BEFORE}"

  local SNAPSHOT_SUFFIX
  SNAPSHOT_SUFFIX="$(date -u '+%Y%m%dT%H%M%SZ')-$$"
  local FINAL_DIRECTORY="${DESTINATION_ROOT}/mailcli-task-history-${SNAPSHOT_SUFFIX}"
  STAGING_DIRECTORY="${DESTINATION_ROOT}/.mailcli-task-history-${SNAPSHOT_SUFFIX}.incomplete"
  [[ ! -e "${FINAL_DIRECTORY}" && ! -e "${STAGING_DIRECTORY}" ]] ||
    fail 'Snapshot destination already exists'
  mkdir "${STAGING_DIRECTORY}"
  chmod 700 "${STAGING_DIRECTORY}"

  local RELATIVE_PATH
  while IFS= read -r RELATIVE_PATH; do
    mkdir -p "$(dirname "${STAGING_DIRECTORY}/${RELATIVE_PATH}")"
    chmod 700 "$(dirname "${STAGING_DIRECTORY}/${RELATIVE_PATH}")"
    [[ ! -e "${STAGING_DIRECTORY}/${RELATIVE_PATH}" ]] ||
      fail "Snapshot path already exists: ${RELATIVE_PATH}"
    cp -p "${MAILCLI_ROOT}/${RELATIVE_PATH}" "${STAGING_DIRECTORY}/${RELATIVE_PATH}"
    chmod 600 "${STAGING_DIRECTORY}/${RELATIVE_PATH}"
  done <"${PATH_LIST}"
  write_manifest "${STAGING_DIRECTORY}" "${PATH_LIST}" "${STAGING_DIRECTORY}/${MANIFEST_NAME}"

  collect_task_paths "${PATH_LIST_AFTER}"
  validate_task_sources "${PATH_LIST_AFTER}"
  write_manifest "${MAILCLI_ROOT}" "${PATH_LIST_AFTER}" "${SOURCE_MANIFEST_AFTER}"
  cmp -s "${PATH_LIST}" "${PATH_LIST_AFTER}" || fail 'Task history file set changed during export'
  cmp -s "${SOURCE_MANIFEST_BEFORE}" "${SOURCE_MANIFEST_AFTER}" ||
    fail 'Task history content changed during export'
  verify_snapshot "${STAGING_DIRECTORY}" >/dev/null

  mv "${STAGING_DIRECTORY}" "${FINAL_DIRECTORY}"
  STAGING_DIRECTORY=""
  verify_snapshot "${FINAL_DIRECTORY}"
  printf 'snapshot_created=%s\n' "${FINAL_DIRECTORY}"
}

COMMAND="${1:-}"
case "${COMMAND}" in
  create)
    [[ "$#" -le 2 ]] || fail 'create accepts at most one destination root'
    create_snapshot "${2:-}"
    ;;
  verify)
    [[ "$#" -eq 2 ]] || fail 'verify requires exactly one snapshot directory'
    verify_snapshot "$2"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
SCHEMA_VERSION=1
DOCTOR_TTL_SECONDS=300
COMMAND=""
COMMANDS=""
COMMANDS_SET=0
NORMALIZED_COMMANDS=""
BINARY="${MAILCLI_PREFLIGHT_BINARY:-}"
CACHE_ROOT="${MAILCLI_PREFLIGHT_CACHE_DIR:-${XDG_CACHE_HOME:-${HOME}/Library/Caches}/mailcli/preflight}"
REFRESH="${MAILCLI_PREFLIGHT_REFRESH:-0}"

usage() {
  printf '%s\n' \
    'Usage:' \
    '  mailcli-preflight.sh capabilities [--commands ID,ID,...] [--binary PATH] [--cache-dir DIR] [--refresh]' \
    '  mailcli-preflight.sh doctor [--binary PATH] [--cache-dir DIR] [--refresh]' \
    '  mailcli-preflight.sh invalidate [--binary PATH] [--cache-dir DIR]'
}

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    capabilities|doctor|invalidate)
      [[ -z "${COMMAND}" ]] || fail "Only one preflight command is allowed"
      COMMAND="$1"
      ;;
    --binary)
      [[ "$#" -ge 2 ]] || fail "--binary requires a path"
      BINARY="$2"
      shift
      ;;
    --cache-dir)
      [[ "$#" -ge 2 ]] || fail "--cache-dir requires a path"
      CACHE_ROOT="$2"
      shift
      ;;
    --commands)
      [[ "$#" -ge 2 ]] || fail "--commands requires a comma-separated command set"
      COMMANDS="$2"
      COMMANDS_SET=1
      shift
      ;;
    --refresh)
      REFRESH=1
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      fail "Unknown preflight argument: $1"
      ;;
  esac
  shift
done

normalize_commands() {
  local value
  local index
  local other
  local -a values=()
  [[ -n "${COMMANDS}" && "${COMMANDS}" != ,* && "${COMMANDS}" != *, && "${COMMANDS}" != *,,* ]] ||
    fail "--commands requires non-empty command IDs without duplicates"
  IFS=, read -r -a values <<<"${COMMANDS}"
  (( ${#values[@]} > 0 )) || fail "--commands requires at least one command ID"
  for ((index = 0; index < ${#values[@]}; index++)); do
    value="${values[index]}"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    [[ -n "${value}" && "${value}" != *$'\n'* && "${value}" != *$'\t'* ]] ||
      fail "--commands contains an empty or invalid command ID"
    values[index]="${value}"
  done
  for ((index = 0; index < ${#values[@]}; index++)); do
    for ((other = 0; other < index; other++)); do
      [[ "${values[index]}" != "${values[other]}" ]] ||
        fail "--commands repeats command ID: ${values[index]}"
    done
  done
  NORMALIZED_COMMANDS="$(printf '%s\n' "${values[@]}" | LC_ALL=C sort | paste -sd, -)"
}

[[ -n "${COMMAND}" ]] || {
  usage >&2
  exit 2
}
if [[ "${COMMANDS_SET}" == "1" ]]; then
  [[ "${COMMAND}" == "capabilities" ]] || fail "--commands is only valid with capabilities"
  normalize_commands
fi
[[ "${CACHE_ROOT}" == /* && "${CACHE_ROOT}" != "/" ]] ||
  fail "Cache directory must be a non-root absolute path: ${CACHE_ROOT}"

if [[ -z "${BINARY}" ]]; then
  BINARY="$(command -v mailcli || true)"
fi
if [[ -z "${BINARY}" && -x "${SCRIPT_ROOT}/bin/mailcli" ]]; then
  BINARY="${SCRIPT_ROOT}/bin/mailcli"
fi
[[ -n "${BINARY}" && -f "${BINARY}" && -x "${BINARY}" && ! -L "${BINARY}" ]] ||
  fail "MailCLI binary is missing, non-executable, or a symbolic link: ${BINARY:-<none>}"

mkdir -p "${CACHE_ROOT}"
[[ ! -L "${CACHE_ROOT}" && -d "${CACHE_ROOT}" ]] ||
  fail "Cache directory must be a real directory: ${CACHE_ROOT}"
chmod 0700 "${CACHE_ROOT}"

BINARY_HASH="$(shasum -a 256 "${BINARY}" | awk '{print $1}')"
[[ "${BINARY_HASH}" =~ ^[[:xdigit:]]{64}$ ]] || fail "Could not fingerprint the MailCLI binary"
VERSION_OUTPUT="$("${BINARY}" version 2>/dev/null || true)"
[[ "${VERSION_OUTPUT}" =~ ^mailcli\ [0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "MailCLI binary version output is invalid"

FULL_CAPABILITIES_CACHE="${CACHE_ROOT}/capabilities-${BINARY_HASH}-schema-${SCHEMA_VERSION}.json"
if [[ "${COMMANDS_SET}" == "1" ]]; then
  COMMANDS_HASH="$(printf '%s' "${NORMALIZED_COMMANDS}" | shasum -a 256 | awk '{print $1}')"
  CAPABILITIES_CACHE="${CACHE_ROOT}/capabilities-${BINARY_HASH}-schema-${SCHEMA_VERSION}-commands-${COMMANDS_HASH}.json"
else
  CAPABILITIES_CACHE="${FULL_CAPABILITIES_CACHE}"
fi
DOCTOR_CACHE="${CACHE_ROOT}/doctor-${BINARY_HASH}-schema-${SCHEMA_VERSION}.json"

file_mtime() {
  stat -f '%m' "$1" 2>/dev/null || stat -c '%Y' "$1"
}

cache_has_contract() {
  local path="$1"
  [[ -f "${path}" && ! -L "${path}" && -s "${path}" ]] || return 1
  grep -Eq '"schema_version"[[:space:]]*:[[:space:]]*1' "${path}" || return 1
  grep -Fq '"ok":true' "${path}" || return 1
  grep -Eq '"command"[[:space:]]*:[[:space:]]*"'"${COMMAND}"'"' "${path}" || return 1
}

doctor_cache_fresh() {
  local path="$1"
  local now modified
  now="$(date +%s)"
  modified="$(file_mtime "${path}")"
  [[ "${modified}" =~ ^[0-9]+$ && "${now}" =~ ^[0-9]+$ ]] || return 1
  (( now >= modified && now - modified <= DOCTOR_TTL_SECONDS ))
}

cache_is_usable() {
  local path="$1"
  cache_has_contract "${path}" || return 1
  if [[ "${COMMAND}" == "doctor" ]]; then
    doctor_cache_fresh "${path}"
  fi
}

invalidate() {
  rm -f "${FULL_CAPABILITIES_CACHE}" "${DOCTOR_CACHE}"
  local SELECTED_CACHE
  for SELECTED_CACHE in "${CACHE_ROOT}/capabilities-${BINARY_HASH}-schema-${SCHEMA_VERSION}-commands-"*.json; do
    [[ -f "${SELECTED_CACHE}" && ! -L "${SELECTED_CACHE}" ]] || continue
    rm -f -- "${SELECTED_CACHE}"
  done
}

if [[ "${COMMAND}" == "invalidate" ]]; then
  invalidate
  exit 0
fi

if [[ "${COMMAND}" == "capabilities" ]]; then
  CACHE_PATH="${CAPABILITIES_CACHE}"
else
  CACHE_PATH="${DOCTOR_CACHE}"
fi

if [[ "${REFRESH}" != "1" ]] && cache_is_usable "${CACHE_PATH}"; then
  cat "${CACHE_PATH}"
  exit 0
fi

rm -f "${CACHE_PATH}"
TEMPORARY="$(mktemp "${CACHE_ROOT}/.${COMMAND}.XXXXXX")"
cleanup() {
  rm -f "${TEMPORARY}"
}
trap cleanup EXIT

set +e
if [[ "${COMMANDS_SET}" == "1" ]]; then
  "${BINARY}" capabilities --commands "${NORMALIZED_COMMANDS}" --json >"${TEMPORARY}"
else
  "${BINARY}" "${COMMAND}" --json >"${TEMPORARY}"
fi
COMMAND_STATUS=$?
set -e
if [[ "${COMMAND_STATUS}" -ne 0 ]]; then
  cat "${TEMPORARY}"
  exit "${COMMAND_STATUS}"
fi
cache_has_contract "${TEMPORARY}" || {
  cat "${TEMPORARY}" >&2
  fail "${COMMAND} returned an invalid schema-${SCHEMA_VERSION} envelope"
}
chmod 0600 "${TEMPORARY}"
mv -f "${TEMPORARY}" "${CACHE_PATH}"
trap - EXIT
cat "${CACHE_PATH}"

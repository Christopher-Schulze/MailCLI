#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
REPOSITORY_SKILL="${MAILCLI_ROOT}/skills/mailcli"
INSTALLED_SKILL="${MAILCLI_SKILL_DESTINATION:-}"
SNAPSHOT_ROOT=""
DIFF_OUTPUT=""

usage() {
  printf '%s\n' \
    'Usage:' \
    '  report-skill-drift.sh [--repository PATH] [--installed PATH]' \
    '  report-skill-drift.sh --help'
}

fail_usage() {
  printf '%s\n' "$1" >&2
  usage >&2
  exit 2
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --repository)
      [[ "$#" -ge 2 ]] || fail_usage "--repository requires a path"
      REPOSITORY_SKILL="$2"
      shift
      ;;
    --installed)
      [[ "$#" -ge 2 ]] || fail_usage "--installed requires a path"
      INSTALLED_SKILL="$2"
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      fail_usage "Unknown option: $1"
      ;;
  esac
  shift
done

if [[ -z "${INSTALLED_SKILL}" ]]; then
  [[ -n "${HOME:-}" ]] || fail_usage 'The installed path is missing and HOME is unset'
  INSTALLED_SKILL="${HOME}/.agents/skills/mailcli"
fi

require_absolute_path() {
  local label="$1"
  local path="$2"
  [[ "${path}" == /* && "${path}" != "/" ]] ||
    fail_usage "${label} path must be a non-root absolute path: ${path}"
  [[ "${path}" != *$'\t'* && "${path}" != *$'\n'* && "${path}" != *$'\r'* ]] ||
    fail_usage "${label} path must not contain control characters"
}

require_absolute_path repository "${REPOSITORY_SKILL}"
require_absolute_path installed "${INSTALLED_SKILL}"

if [[ ! -d "${REPOSITORY_SKILL}" || -L "${REPOSITORY_SKILL}" ]]; then
  printf 'repository_skill=%s\nstatus=invalid\nmessage=Repository skill must be a real directory.\n' \
    "${REPOSITORY_SKILL}" >&2
  exit 2
fi
if find "${REPOSITORY_SKILL}" -type l -print -quit | grep -q .; then
  printf 'repository_skill=%s\nstatus=invalid\nmessage=Repository skill contains a symbolic link.\n' \
    "${REPOSITORY_SKILL}" >&2
  exit 2
fi

printf 'repository_skill=%s\n' "${REPOSITORY_SKILL}"
printf 'installed_skill=%s\n' "${INSTALLED_SKILL}"

if [[ ! -e "${INSTALLED_SKILL}" && ! -L "${INSTALLED_SKILL}" ]]; then
  printf '%s\n' \
    'status=missing' \
    'message=Installed skill is missing.' \
    'reconcile=Run scripts/build/install-local.sh or mailcli update, then rerun this check.'
  exit 1
fi
if [[ ! -d "${INSTALLED_SKILL}" || -L "${INSTALLED_SKILL}" ]]; then
  printf '%s\n' \
    'status=mismatch' \
    'message=Installed skill is not a real directory.' \
    'reconcile=Run scripts/build/install-local.sh or mailcli update, then rerun this check.'
  exit 1
fi
if find "${INSTALLED_SKILL}" -type l -print -quit | grep -q .; then
  printf '%s\n' \
    'status=mismatch' \
    'message=Installed skill contains a symbolic link.' \
    'reconcile=Run scripts/build/install-local.sh or mailcli update, then rerun this check.'
  exit 1
fi

SNAPSHOT_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-skill-drift.XXXXXX")"
cleanup_snapshot() {
  if [[ "${SNAPSHOT_ROOT}" == *"/mailcli-skill-drift."* && -d "${SNAPSHOT_ROOT}" ]]; then
    rm -rf "${SNAPSHOT_ROOT}"
  fi
}
trap cleanup_snapshot EXIT

mkdir "${SNAPSHOT_ROOT}/repository" "${SNAPSHOT_ROOT}/installed"
cp -R "${REPOSITORY_SKILL}/." "${SNAPSHOT_ROOT}/repository/"
cp -R "${INSTALLED_SKILL}/." "${SNAPSHOT_ROOT}/installed/"

compare_trees() {
  local status
  DIFF_OUTPUT=""
  if DIFF_OUTPUT="$(diff -qr "$1" "$2" 2>&1)"; then
    return 0
  else
    status=$?
    return "${status}"
  fi
}

report_unstable() {
  printf '%s\n' \
    'status=unstable' \
    'message=The skill changed while it was being checked.' \
    'reconcile=Stop concurrent installation, then rerun this read-only check.'
  exit 1
}

if compare_trees "${REPOSITORY_SKILL}" "${SNAPSHOT_ROOT}/repository"; then
  :
else
  compare_status=$?
  [[ "${compare_status}" -eq 1 ]] && report_unstable
  printf 'status=unavailable\nmessage=Could not snapshot the repository skill: %s\n' "${DIFF_OUTPUT}" >&2
  exit 2
fi
if compare_trees "${INSTALLED_SKILL}" "${SNAPSHOT_ROOT}/installed"; then
  :
else
  compare_status=$?
  [[ "${compare_status}" -eq 1 ]] && report_unstable
  printf 'status=unavailable\nmessage=Could not snapshot the installed skill: %s\n' "${DIFF_OUTPUT}" >&2
  exit 2
fi

if compare_trees "${SNAPSHOT_ROOT}/repository" "${SNAPSHOT_ROOT}/installed"; then
  :
else
  compare_status=$?
  if [[ "${compare_status}" -eq 1 ]]; then
    printf '%s\n' \
      'status=mismatch' \
      'message=Installed skill differs from the repository skill.' \
      'reconcile=Run scripts/build/install-local.sh or mailcli update, then rerun this check.'
    if [[ -n "${DIFF_OUTPUT}" ]]; then
      printf 'diff=%s\n' "${DIFF_OUTPUT//$'\n'/; }"
    fi
    exit 1
  fi
  printf 'status=unavailable\nmessage=Could not compare skill snapshots: %s\n' "${DIFF_OUTPUT}" >&2
  exit 2
fi

if compare_trees "${REPOSITORY_SKILL}" "${SNAPSHOT_ROOT}/repository"; then
  :
else
  report_unstable
fi
if compare_trees "${INSTALLED_SKILL}" "${SNAPSHOT_ROOT}/installed"; then
  :
else
  report_unstable
fi

printf '%s\n' \
  'status=match' \
  'message=Repository and installed skill trees are byte-identical.'

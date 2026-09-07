#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXPORT_TOOL="${MAILCLI_ROOT}/scripts/utils/export-task-history.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-task-history-export.XXXXXX")"

cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *'/mailcli-task-history-export.'* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT
chmod 700 "${TEST_ROOT}"

CREATE_OUTPUT="$("${EXPORT_TOOL}" create "${TEST_ROOT}")"
SNAPSHOT_DIRECTORY="$(printf '%s\n' "${CREATE_OUTPUT}" | sed -n 's/^snapshot_created=//p')"
[[ -n "${SNAPSHOT_DIRECTORY}" && -d "${SNAPSHOT_DIRECTORY}" ]] || {
  printf 'Task-history exporter did not create a snapshot\n' >&2
  exit 1
}
"${EXPORT_TOOL}" verify "${SNAPSHOT_DIRECTORY}" >/dev/null

SOURCE_COUNT="$((1 + $(find "${MAILCLI_ROOT}/docs/tasks" -type f -name '*.md' | wc -l | tr -d ' ')))"
MANIFEST_COUNT="$(wc -l <"${SNAPSHOT_DIRECTORY}/MANIFEST.sha256" | tr -d ' ')"
[[ "${MANIFEST_COUNT}" -eq "${SOURCE_COUNT}" ]] || {
  printf 'Snapshot file count = %s, source count = %s\n' "${MANIFEST_COUNT}" "${SOURCE_COUNT}" >&2
  exit 1
}
for TASK_ID in 134 135 172 174 175 176 177 178; do
  if ! cut -f 2- "${SNAPSHOT_DIRECTORY}/MANIFEST.sha256" |
    grep -Eq "^docs/tasks/(done/)?${TASK_ID}-[a-z0-9-]+\\.md$"; then
    printf 'Snapshot manifest is missing TASK %s\n' "${TASK_ID}" >&2
    exit 1
  fi
done
if find "${SNAPSHOT_DIRECTORY}" -type f -name 'README.md' -print -quit | grep -q .; then
  printf 'Snapshot copied a non-task repository file\n' >&2
  exit 1
fi

printf '\ncorruption fixture\n' >>"${SNAPSHOT_DIRECTORY}/docs/tasks.md"
if "${EXPORT_TOOL}" verify "${SNAPSHOT_DIRECTORY}" >/dev/null 2>&1; then
  printf 'Snapshot verification accepted modified task history\n' >&2
  exit 1
fi

INSECURE_ROOT="${TEST_ROOT}/insecure"
mkdir "${INSECURE_ROOT}"
chmod 755 "${INSECURE_ROOT}"
if "${EXPORT_TOOL}" create "${INSECURE_ROOT}" >/dev/null 2>&1; then
  printf 'Task-history exporter accepted a group/world-accessible destination\n' >&2
  exit 1
fi

printf 'Task-history export passed: exact private file set, owner-only modes, manifest verification, and corruption rejection\n'

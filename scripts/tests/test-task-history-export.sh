#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
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
MANIFEST_PATHS="${TEST_ROOT}/manifest-paths.txt"
cut -f 2- "${SNAPSHOT_DIRECTORY}/MANIFEST.sha256" >"${MANIFEST_PATHS}"
MANIFEST_MISSING=0
if ! grep -Fxq -- "docs/tasks.md" "${MANIFEST_PATHS}"; then
  printf 'Snapshot manifest is missing docs/tasks.md\n' >&2
  MANIFEST_MISSING=1
fi
while IFS= read -r -d '' SOURCE_FILE; do
  RELATIVE_PATH="${SOURCE_FILE#"${MAILCLI_ROOT}/"}"
  if ! grep -Fxq -- "${RELATIVE_PATH}" "${MANIFEST_PATHS}"; then
    printf 'Snapshot manifest is missing %s\n' "${RELATIVE_PATH}" >&2
    MANIFEST_MISSING=1
  fi
done < <(find "${MAILCLI_ROOT}/docs/tasks" -type f -name '*.md' -print0)
[[ "${MANIFEST_MISSING}" -eq 0 ]] || exit 1
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

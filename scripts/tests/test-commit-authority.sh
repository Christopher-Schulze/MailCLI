#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SELF_PATH="${MAILCLI_ROOT}/scripts/tests/test-commit-authority.sh"

find_commit_commands() {
  grep -En \
    -e '(^|[;&|({][[:space:]]*|[[:space:]]+)(command[[:space:]]+)?([^[:space:]]*/)?git([[:space:]]+(-C|-c|--git-dir|--work-tree)[[:space:]]+[^[:space:]]+)*[[:space:]]+(add|commit)([[:space:]]|$)' \
    "$@"
}

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-commit-authority.XXXXXX")"
cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-commit-authority."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

DETECTION_FIXTURE="${TEST_ROOT}/must-detect.sh"
printf '%s\n' \
  'git add --all' \
  'git -C "${ROOT}" commit -m "TASK"' \
  'command git -c user.name=test commit --allow-empty -m test' >"${DETECTION_FIXTURE}"
DETECTED_FIXTURE_LINES="$(find_commit_commands "${DETECTION_FIXTURE}" | wc -l)"
DETECTED_FIXTURE_LINES="${DETECTED_FIXTURE_LINES//[[:space:]]/}"
if [[ "${DETECTED_FIXTURE_LINES}" != 3 ]]; then
  printf 'Commit-authority detector missed a fixture: found %s of 3\n' \
    "${DETECTED_FIXTURE_LINES}" >&2
  exit 1
fi

FAILING_TEST="${TEST_ROOT}/failing-test.sh"
ORCHESTRATOR="${TEST_ROOT}/orchestrator.sh"
COMMIT_SENTINEL="${TEST_ROOT}/commit-step-reached"
printf '%s\n' '#!/usr/bin/env bash' 'exit 23' >"${FAILING_TEST}"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'bash "$1"' \
  'printf commit-step-reached >"$2"' >"${ORCHESTRATOR}"
FAILURE_STATUS=0
bash "${ORCHESTRATOR}" "${FAILING_TEST}" "${COMMIT_SENTINEL}" || FAILURE_STATUS=$?
if [[ "${FAILURE_STATUS}" != 23 || -e "${COMMIT_SENTINEL}" ]]; then
  printf 'Test orchestration lost status 23 or continued to its commit step: status=%s sentinel=%s\n' \
    "${FAILURE_STATUS}" "$([[ -e "${COMMIT_SENTINEL}" ]] && printf present || printf absent)" >&2
  exit 1
fi

SCAN_FILES=()
while IFS= read -r -d '' FILE_PATH; do
  if [[ "${FILE_PATH}" != "${SELF_PATH}" ]]; then
    SCAN_FILES+=("${FILE_PATH}")
  fi
done < <(find "${MAILCLI_ROOT}/scripts" -type f -name '*.sh' -print0)
if [[ -d "${MAILCLI_ROOT}/.github/workflows" ]]; then
  while IFS= read -r -d '' FILE_PATH; do
    SCAN_FILES+=("${FILE_PATH}")
  done < <(find "${MAILCLI_ROOT}/.github/workflows" -type f \
    \( -name '*.yml' -o -name '*.yaml' \) -print0)
fi

if COMMIT_COMMANDS="$(find_commit_commands "${SCAN_FILES[@]}")"; then
  printf 'Normal verification or build paths contain staging or commit commands:\n%s\n' \
    "${COMMIT_COMMANDS}" >&2
  exit 1
fi

printf 'Commit authority boundary passed: failed tests preserve status and no normal script or workflow stages or commits\n'

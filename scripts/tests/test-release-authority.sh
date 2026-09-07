#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SELF_PATH="${MAILCLI_ROOT}/scripts/tests/test-release-authority.sh"

find_publication_commands() {
  grep -En \
    -e '(^|[;&|({][[:space:]]*|[[:space:]]+)(command[[:space:]]+)?([^[:space:]]*/)?git([[:space:]]+(-C|-c|--git-dir|--work-tree)[[:space:]]+[^[:space:]]+)*[[:space:]]+(push|tag|update-ref|send-pack)([[:space:]]|$)' \
    -e '(^|[;&|({][[:space:]]*|[[:space:]]+)(command[[:space:]]+)?([^[:space:]]*/)?gh[[:space:]]+release[[:space:]]+(create|delete|edit|upload)([[:space:]]|$)' \
    -e '(^|[;&|({][[:space:]]*|[[:space:]]+)(command[[:space:]]+)?([^[:space:]]*/)?gh[[:space:]]+(api|workflow[[:space:]]+run)([[:space:]]|$)' \
    -e '(api\.github\.com|uploads\.github\.com)' \
    -e 'uses:[[:space:]]*(actions/create-release|ncipollo/release-action|softprops/action-gh-release|svenstaro/upload-release-action)' \
    "$@"
}

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-release-authority.XXXXXX")"
cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-release-authority."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

DETECTION_FIXTURE="${TEST_ROOT}/must-fail.sh"
printf '%s\n' \
  'git -C "${ROOT}" push origin main' \
  'git tag -a "${TAG}" -m release' \
  'gh release upload "${TAG}" artifact' \
  'gh api --method POST repos/example/project/releases' \
  'curl https://api.github.com/repos/example/project/releases' \
  'uses: softprops/action-gh-release@v2' >"${DETECTION_FIXTURE}"
DETECTED_FIXTURE_LINES="$(find_publication_commands "${DETECTION_FIXTURE}" | wc -l)"
DETECTED_FIXTURE_LINES="${DETECTED_FIXTURE_LINES//[[:space:]]/}"
if [[ "${DETECTED_FIXTURE_LINES}" != 6 ]]; then
  printf 'Release-authority detector missed a publication fixture: found %s of 6\n' \
    "${DETECTED_FIXTURE_LINES}" >&2
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

if PUBLICATION_COMMANDS="$(find_publication_commands "${SCAN_FILES[@]}")"; then
  printf 'Normal verification or build paths contain publication commands:\n%s\n' \
    "${PUBLICATION_COMMANDS}" >&2
  exit 1
fi

printf 'Release authority boundary passed: no publication command in normal scripts or workflows\n'

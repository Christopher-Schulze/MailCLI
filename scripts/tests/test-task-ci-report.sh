#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
REPORT="${MAILCLI_ROOT}/scripts/tests/report-task-ci.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-task-ci-report.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'case "${1:-} ${2:-}" in' \
  '  "run list")' \
  '    [[ " $* " == *" --commit ${MAILCLI_CI_TEST_SHA} "* ]] || exit 91' \
  '    [[ " $* " == *" --workflow ci.yml "* ]] || exit 91' \
  '    [[ " $* " == *" --event push "* ]] || exit 91' \
  '    [[ " $* " == *" --branch main "* ]] || exit 91' \
  '    printf "%s\n" "${MAILCLI_CI_TEST_LIST}" ;;' \
  '  "run view") printf "%s\n" "${MAILCLI_CI_TEST_VIEW}" ;;' \
  '  *) exit 91 ;;' \
  'esac' >"${TEST_ROOT}/gh"
chmod 755 "${TEST_ROOT}/gh"

SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
OLD_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
LIST="$(jq -nc --arg sha "${SHA}" '[{databaseId:123, headSha:$sha,
  status:"completed", conclusion:"success", event:"push", headBranch:"main",
  name:"ci", attempt:1, createdAt:"2026-09-23T00:00:00Z",
  url:"https://github.com/example/repo/actions/runs/123"}]')"
VIEW="$(jq -nc --arg sha "${SHA}" '{databaseId:123, headSha:$sha,
  status:"completed", conclusion:"success", event:"push", headBranch:"main",
  name:"ci", attempt:1, url:"https://github.com/example/repo/actions/runs/123"}')"

check_case() {
  local NAME="$1"
  local EXPECTED_EXIT="$2"
  local EXPECTED_MARKER="$3"
  local RUN_LIST="$4"
  local RUN_VIEW="$5"
  local OUTPUT
  local ACTUAL_EXIT=0
  OUTPUT="$(MAILCLI_CI_TEST_SHA="${SHA}" MAILCLI_CI_TEST_LIST="${RUN_LIST}" \
    MAILCLI_CI_TEST_VIEW="${RUN_VIEW}" PATH="${TEST_ROOT}:${PATH}" \
    "${REPORT}" "${SHA}" 2>&1)" || ACTUAL_EXIT=$?
  if [[ "${ACTUAL_EXIT}" -ne "${EXPECTED_EXIT}" ||
    "${OUTPUT}" != *"${EXPECTED_MARKER}"* ]]; then
    printf '%s: exit %s, output %s\n' "${NAME}" "${ACTUAL_EXIT}" "${OUTPUT}" >&2
    exit 1
  fi
}

check_case success 0 'ci_receipt=success' "${LIST}" "${VIEW}"
check_case mismatched_sha 3 'ci_reason=list_identity_mismatch' \
  "$(jq --arg sha "${OLD_SHA}" '.[0].headSha = $sha' <<<"${LIST}")" "${VIEW}"
check_case missing_run 3 'ci_reason=missing_run' '[]' "${VIEW}"
check_case stale_view 3 'ci_reason=view_identity_mismatch' \
  "$(jq '.[0].attempt = 2' <<<"${LIST}")" "${VIEW}"
check_case false_success 3 'ci_reason=inconsistent_status' "${LIST}" \
  "$(jq '.status = "in_progress" | .conclusion = "success"' <<<"${VIEW}")"
check_case failed 1 'ci_receipt=failed' "${LIST}" \
  "$(jq '.conclusion = "failure"' <<<"${VIEW}")"
check_case cancelled 1 'ci_receipt=failed' "${LIST}" \
  "$(jq '.conclusion = "cancelled"' <<<"${VIEW}")"
check_case pending 2 'ci_receipt=pending' "${LIST}" \
  "$(jq '.status = "in_progress" | .conclusion = ""' <<<"${VIEW}")"

printf 'Task CI receipt passed: exact SHA and run identity, stale and missing runs, false success, failure, cancellation, and pending status\n'

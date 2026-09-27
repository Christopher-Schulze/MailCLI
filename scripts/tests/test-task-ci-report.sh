#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
REPORT="${MAILCLI_ROOT}/scripts/tests/report-task-ci.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-task-ci-report.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
QUERY_ROOT="${TEST_ROOT}/query-repository"
mkdir "${QUERY_ROOT}"
git -C "${QUERY_ROOT}" init -q -b main
git -C "${QUERY_ROOT}" remote add origin https://github.com/example/repo.git

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  '[[ -z "${MAILCLI_CI_TEST_LOG:-}" ]] || printf "%s\n" "$*" >>"${MAILCLI_CI_TEST_LOG}"' \
  'case "${1:-} ${2:-}" in' \
  '  "run list")' \
  '    [[ "${MAILCLI_CI_TEST_LIST_STATUS:-0}" == 0 ]] || exit "${MAILCLI_CI_TEST_LIST_STATUS}"' \
  '    if [[ -n "${MAILCLI_CI_TEST_RESPONSES:-}" ]]; then' \
  '      while [[ "$#" -gt 0 && "$1" != --commit ]]; do shift; done' \
  '      cat "${MAILCLI_CI_TEST_RESPONSES}/$2.list.json"; exit' \
  '    fi' \
  '    [[ " $* " == *" --commit ${MAILCLI_CI_TEST_SHA} "* ]] || exit 91' \
  '    [[ " $* " == *" --workflow ci.yml "* ]] || exit 91' \
  '    [[ " $* " == *" --event push "* ]] || exit 91' \
  '    [[ " $* " == *" --branch main "* ]] || exit 91' \
  '    printf "%s\n" "${MAILCLI_CI_TEST_LIST}" ;;' \
  '  "run view")' \
  '    [[ "${MAILCLI_CI_TEST_VIEW_STATUS:-0}" == 0 ]] || exit "${MAILCLI_CI_TEST_VIEW_STATUS}"' \
  '    if [[ -n "${MAILCLI_CI_TEST_RESPONSES:-}" ]]; then' \
  '      cat "${MAILCLI_CI_TEST_RESPONSES}/$3.view.json"; exit' \
  '    fi' \
  '    [[ "$3" == 123 ]] || exit 91' \
  '    printf "%s\n" "${MAILCLI_CI_TEST_VIEW}" ;;' \
  '  *) exit 91 ;;' \
  'esac' >"${TEST_ROOT}/gh"
chmod 755 "${TEST_ROOT}/gh"

SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
OLD_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
LIST="$(jq -nc --arg sha "${SHA}" '[{databaseId:123, headSha:$sha,
  status:"completed", conclusion:"success", event:"push", headBranch:"main",
  name:"ci", attempt:1, workflowDatabaseId:321, createdAt:"2026-09-23T00:00:00Z",
  url:"https://github.com/example/repo/actions/runs/123"}]')"
VIEW="$(jq -nc --arg sha "${SHA}" '{databaseId:123, headSha:$sha,
  status:"completed", conclusion:"success", event:"push", headBranch:"main",
  name:"ci", attempt:1, workflowDatabaseId:321, url:"https://github.com/example/repo/actions/runs/123"}')"

check_case() {
  local NAME="$1"
  local EXPECTED_EXIT="$2"
  local EXPECTED_MARKER="$3"
  local RUN_LIST="$4"
  local RUN_VIEW="$5"
  local OUTPUT
  local ACTUAL_EXIT=0
  OUTPUT="$(MAILCLI_ROOT="${QUERY_ROOT}" MAILCLI_CI_TEST_SHA="${SHA}" MAILCLI_CI_TEST_LIST="${RUN_LIST}" \
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
check_case ambiguous 3 'ci_reason=ambiguous_runs' "$(jq '. + .' <<<"${LIST}")" "${VIEW}"
check_case malformed 3 'ci_reason=malformed_run_list' '{bad' "${VIEW}"
check_case object_list 3 'ci_reason=malformed_run_list' '{}' "${VIEW}"
check_case unsupported 3 'ci_reason=unsupported_status' "${LIST}" "$(jq '.status = "invented"' <<<"${VIEW}")"
check_case incomplete 3 'ci_reason=incomplete_conclusion' "${LIST}" "$(jq '.conclusion = null' <<<"${VIEW}")"
check_case wrong_id 3 'ci_reason=view_identity_mismatch' "${LIST}" "$(jq '.databaseId = 124' <<<"${VIEW}")"
check_case wrong_event 3 'ci_reason=list_identity_mismatch' "$(jq '.[0].event = "pull_request"' <<<"${LIST}")" "${VIEW}"
check_case wrong_branch 3 'ci_reason=view_identity_mismatch' "${LIST}" "$(jq '.headBranch = "other"' <<<"${VIEW}")"
check_case wrong_workflow 3 'ci_reason=view_identity_mismatch' "${LIST}" "$(jq '.workflowDatabaseId = 322' <<<"${VIEW}")"
check_case wrong_repository 3 'ci_reason=view_identity_mismatch' "${LIST}" "$(jq '.url = "https://github.com/other/repo/actions/runs/123"' <<<"${VIEW}")"
MAILCLI_CI_TEST_LIST_STATUS=23 check_case list_unavailable 3 'ci_reason=run_list_unavailable' "${LIST}" "${VIEW}"
MAILCLI_CI_TEST_VIEW_STATUS=23 check_case view_unavailable 3 'ci_reason=run_view_unavailable' "${LIST}" "${VIEW}"
STATUS=0
/bin/bash "${REPORT}" >"${TEST_ROOT}/usage.log" 2>&1 || STATUS=$?
[[ "${STATUS}" == 64 ]]
mkdir "${TEST_ROOT}/missing-tools"
STATUS=0
PATH="${TEST_ROOT}/missing-tools" /bin/bash "${REPORT}" "${SHA}" >"${TEST_ROOT}/missing.log" 2>&1 || STATUS=$?
[[ "${STATUS}" == 69 ]]
grep -Fqx 'ci_reason=missing_gh' "${TEST_ROOT}/missing.log"
ln -s "${TEST_ROOT}/gh" "${TEST_ROOT}/missing-tools/gh"
STATUS=0
PATH="${TEST_ROOT}/missing-tools" /bin/bash "${REPORT}" "${SHA}" >"${TEST_ROOT}/missing.log" 2>&1 || STATUS=$?
[[ "${STATUS}" == 69 ]]
grep -Fqx 'ci_reason=missing_jq' "${TEST_ROOT}/missing.log"

REPOSITORY="${TEST_ROOT}/repository"
RESPONSES="${TEST_ROOT}/responses"
mkdir -p "${REPOSITORY}/docs/tasks/done" "${RESPONSES}"
git -C "${REPOSITORY}" init -q -b main
git -C "${REPOSITORY}" remote add origin https://github.com/example/repo.git
git -C "${REPOSITORY}" config user.name 'MailCLI CI Test'
git -C "${REPOSITORY}" config user.email 'mailcli-ci-test@example.invalid'
TREE="$(git -C "${REPOSITORY}" mktree </dev/null)"
BASE="$(printf 'CI fixture baseline\n' | git -C "${REPOSITORY}" commit-tree "${TREE}")"
GROUP="$(printf 'TASK 508, 513: CI group fixture\n' | git -C "${REPOSITORY}" commit-tree "${TREE}" -p "${BASE}")"
git -C "${REPOSITORY}" checkout -q --detach "${GROUP}"
WINDOW="$(jq -nc --arg base "${BASE}" '{schema:1,base_sha:$base,started_utc:"2026-09-26 11:36:06 UTC",initial_done:[],initial_open:["508","513"]}')"
printf '# TASK 512: Fixture window\n\n- ci_run_window: %s\n' "${WINDOW}" >"${REPOSITORY}/docs/tasks/done/512-window.md"
# Window carrier is historical, not a newly closed product member.
WINDOW="$(jq -c '.initial_done = ["512"]' <<<"${WINDOW}")"
printf '# TASK 512: Fixture window\n\n- ci_run_window: %s\n' "${WINDOW}" >"${REPOSITORY}/docs/tasks/done/512-window.md"
printf '# Tasks\n\n## Active\n\n## Queue\n\n## Blocked\n\n## Done\n' >"${REPOSITORY}/docs/tasks.md"
for ID in 508 513; do
  RECORD="$(jq -nc --arg base "${BASE}" --arg id "${ID}" --arg group "${GROUP}" \
    '{schema:1,run_base:$base,task_id:$id,product_commit:$group,closed_head:$group,group_head:$group,ci_state:"success",run_id:123,attempt:1,url:"https://github.com/example/repo/actions/runs/123"}')"
  printf '# TASK %s: Fixture\n\n- ci_record: %s\n' "${ID}" "${RECORD}" >"${REPOSITORY}/docs/tasks/done/${ID}-fixture.md"
done
jq --arg sha "${GROUP}" '.[0].headSha = $sha' <<<"${LIST}" >"${RESPONSES}/${GROUP}.list.json"
jq --arg sha "${GROUP}" '.headSha = $sha' <<<"${VIEW}" >"${RESPONSES}/123.view.json"
check_run() {
  local MODE="$1" EXPECTED="$2" NEEDLE="$3" STATUS=0
  : >"${TEST_ROOT}/calls.log"
  MAILCLI_ROOT="${REPOSITORY}" MAILCLI_CI_TEST_RESPONSES="${RESPONSES}" \
    MAILCLI_CI_TEST_LOG="${TEST_ROOT}/calls.log" PATH="${TEST_ROOT}:${PATH}" \
    "${REPORT}" "${MODE}" --run-task 512 >"${TEST_ROOT}/run.log" 2>&1 || STATUS=$?
  [[ "${STATUS}" == "${EXPECTED}" ]] || {
    cat "${TEST_ROOT}/run.log" >&2
    printf 'Expected run status %s, got %s\n' "${EXPECTED}" "${STATUS}" >&2
    exit 1
  }
  grep -Fq "${NEEDLE}" "${TEST_ROOT}/run.log"
}
check_run --completion 0 ci_report_status=0
[[ "$(wc -l <"${TEST_ROOT}/calls.log" | tr -d ' ')" == 2 ]]
cp "${REPOSITORY}/docs/tasks/done/513-fixture.md" "${TEST_ROOT}/saved-record"
rm "${REPOSITORY}/docs/tasks/done/513-fixture.md"
check_run --completion 3 missing_product_mapping_513
cp "${TEST_ROOT}/saved-record" "${REPOSITORY}/docs/tasks/done/513-fixture.md"

RECORD="$(sed -n 's/^- ci_record: //p' "${TEST_ROOT}/saved-record")"
UNPUBLISHED="$(jq '.group_head = null | .ci_state = "unpublished" | .run_id = null | .attempt = null | .url = null' <<<"${RECORD}")"
printf '# TASK 513: Fixture\n\n- ci_record: %s\n' "$(jq -c . <<<"${UNPUBLISHED}")" >"${REPOSITORY}/docs/tasks/done/513-fixture.md"
check_run --pending 2 ci_unpublished=true
cp "${TEST_ROOT}/saved-record" "${REPOSITORY}/docs/tasks/done/513-fixture.md"

NO_CHANGE="$(jq '.task_id = "529" | .product_commit = null | .group_head = null | .ci_state = "not_required" | .run_id = null | .attempt = null | .url = null' <<<"${RECORD}")"
printf '# TASK 529: No change\n\n- ci_record: %s\n' "$(jq -c . <<<"${NO_CHANGE}")" >"${REPOSITORY}/docs/tasks/done/529-no-change.md"
check_run --completion 0 ci_recorded_state=not_required
[[ "$(wc -l <"${TEST_ROOT}/calls.log" | tr -d ' ')" == 2 ]]
FOREIGN="$(printf 'unrelated fixture\n' | git -C "${REPOSITORY}" commit-tree "${TREE}")"
BAD_GROUP="$(jq --arg sha "${FOREIGN}" '.group_head = $sha' <<<"${RECORD}")"
printf '# TASK 513: Fixture\n\n- ci_record: %s\n' "$(jq -c . <<<"${BAD_GROUP}")" >"${REPOSITORY}/docs/tasks/done/513-fixture.md"
check_run --pending 3 group_ancestry_513
cp "${TEST_ROOT}/saved-record" "${REPOSITORY}/docs/tasks/done/513-fixture.md"

for STATE in pending failed unknown; do
  case "${STATE}" in
    pending) jq '.status = "queued" | .conclusion = null' <<<"${VIEW}" ;;
    failed) jq '.conclusion = "failure"' <<<"${VIEW}" ;;
    unknown) jq '.status = "invented"' <<<"${VIEW}" ;;
  esac | jq --arg sha "${GROUP}" '.headSha = $sha' >"${RESPONSES}/123.view.json"
  case "${STATE}" in pending) EXPECTED=2 ;; failed) EXPECTED=1 ;; unknown) EXPECTED=3 ;; esac
  check_run --completion "${EXPECTED}" ci_report_status=
done
jq --arg sha "${GROUP}" '.headSha = $sha' <<<"${VIEW}" >"${RESPONSES}/123.view.json"
STALE_RECORD="$(jq '.attempt = 2' <<<"${RECORD}")"
printf '# TASK 513: Fixture\n\n- ci_record: %s\n' "$(jq -c . <<<"${STALE_RECORD}")" >"${REPOSITORY}/docs/tasks/done/513-fixture.md"
check_run --completion 3 unpersisted_or_stale_group_receipt
cp "${TEST_ROOT}/saved-record" "${REPOSITORY}/docs/tasks/done/513-fixture.md"
INCOMPLETE_WINDOW="$(jq -c '.initial_open += ["538"]' <<<"${WINDOW}")"
printf '# TASK 512: Fixture window\n\n- ci_run_window: %s\n' "${INCOMPLETE_WINDOW}" >"${REPOSITORY}/docs/tasks/done/512-window.md"
check_run --completion 2 incomplete_run_closure_set
printf '# TASK 512: Fixture window\n\n- ci_run_window: %s\n' "${WINDOW}" >"${REPOSITORY}/docs/tasks/done/512-window.md"
SECOND="$(printf 'TASK 500: Descendant repair fixture\n' | git -C "${REPOSITORY}" commit-tree "${TREE}" -p "${GROUP}")"
git -C "${REPOSITORY}" checkout -q --detach "${SECOND}"
SECOND_RECORD="$(jq --arg sha "${SECOND}" '.task_id = "500" | .product_commit = $sha | .closed_head = $sha | .group_head = $sha | .run_id = 124 | .url = "https://github.com/example/repo/actions/runs/124"' <<<"${RECORD}")"
printf '# TASK 500: Repair\n\n- ci_record: %s\n' "$(jq -c . <<<"${SECOND_RECORD}")" >"${REPOSITORY}/docs/tasks/done/500-fixture.md"
jq --arg sha "${SECOND}" '.[0].headSha = $sha | .[0].databaseId = 124 | .[0].url = "https://github.com/example/repo/actions/runs/124"' <<<"${LIST}" >"${RESPONSES}/${SECOND}.list.json"
jq --arg sha "${SECOND}" '.headSha = $sha | .databaseId = 124 | .url = "https://github.com/example/repo/actions/runs/124"' <<<"${VIEW}" >"${RESPONSES}/124.view.json"
check_run --completion 0 ci_report_status=0
[[ "$(wc -l <"${TEST_ROOT}/calls.log" | tr -d ' ')" == 4 ]]
for FAILED_ID in 123 124; do
  FAILED_SHA="${GROUP}"; [[ "${FAILED_ID}" != 124 ]] || FAILED_SHA="${SECOND}"
  jq --arg sha "${FAILED_SHA}" --argjson id "${FAILED_ID}" \
    '.headSha = $sha | .databaseId = $id | .conclusion = "failure" | .url = ("https://github.com/example/repo/actions/runs/" + ($id | tostring))' \
    <<<"${VIEW}" >"${RESPONSES}/${FAILED_ID}.view.json"
  check_run --completion 1 ci_report_status=1
  [[ "$(wc -l <"${TEST_ROOT}/calls.log" | tr -d ' ')" == 4 ]]
  jq --arg sha "${FAILED_SHA}" --argjson id "${FAILED_ID}" \
    '.headSha = $sha | .databaseId = $id | .url = ("https://github.com/example/repo/actions/runs/" + ($id | tostring))' \
    <<<"${VIEW}" >"${RESPONSES}/${FAILED_ID}.view.json"
done
# Preserve the old red witness while remapping covered tasks to its successful descendant repair.
for ID in 508 513; do
  REPAIRED="$(jq --arg id "${ID}" --arg group "${SECOND}" --arg old "${GROUP}" \
    '.task_id = $id | .group_head = $group | .run_id = 124 | .url = "https://github.com/example/repo/actions/runs/124" |
     .previous_failed_receipt = {group_head:$old,run_id:123,attempt:1,url:"https://github.com/example/repo/actions/runs/123",state:"failed",remediation_task:"500"}' <<<"${RECORD}")"
  printf '# TASK %s: Fixture\n\n- ci_record: %s\n' "${ID}" "$(jq -c . <<<"${REPAIRED}")" >"${REPOSITORY}/docs/tasks/done/${ID}-fixture.md"
done
check_run --completion 0 ci_report_status=0
[[ "$(wc -l <"${TEST_ROOT}/calls.log" | tr -d ' ')" == 2 ]]
grep -Fq 'previous_failed_receipt' "${REPOSITORY}/docs/tasks/done/508-fixture.md"
# A final accepted product covers earlier same-task implementations, not sibling commits.
INTERMEDIATE_PRODUCT="$(printf 'TASK 529: Intermediate implementation\n' | git -C "${REPOSITORY}" commit-tree "${TREE}" -p "${SECOND}")"
FINAL_PRODUCT="$(printf 'TASK 529: Final accepted implementation\n' | git -C "${REPOSITORY}" commit-tree "${TREE}" -p "${INTERMEDIATE_PRODUCT}")"
git -C "${REPOSITORY}" checkout -q --detach "${FINAL_PRODUCT}"
FINAL_RECORD="$(jq --arg sha "${FINAL_PRODUCT}" \
  '.product_commit = $sha | .closed_head = $sha | .group_head = null | .ci_state = "unpublished" |
   .run_id = null | .attempt = null | .url = null' <<<"${NO_CHANGE}")"
printf '# TASK 529: Final implementation\n\n- ci_record: %s\n' "$(jq -c . <<<"${FINAL_RECORD}")" >"${REPOSITORY}/docs/tasks/done/529-no-change.md"
check_run --pending 2 ci_unpublished=true
[[ "$(wc -l <"${TEST_ROOT}/calls.log" | tr -d ' ')" == 2 ]]
SIBLING_PRODUCT="$(printf 'TASK 529: Unaccepted sibling implementation\n' | git -C "${REPOSITORY}" commit-tree "${TREE}" -p "${INTERMEDIATE_PRODUCT}")"
MERGED_PRODUCT="$(printf 'Merge fixture branches\n' | git -C "${REPOSITORY}" commit-tree "${TREE}" -p "${FINAL_PRODUCT}" -p "${SIBLING_PRODUCT}")"
git -C "${REPOSITORY}" checkout -q --detach "${MERGED_PRODUCT}"
check_run --pending 3 uncovered_product_commit_529
[[ ! -s "${TEST_ROOT}/calls.log" ]]
git -C "${REPOSITORY}" checkout -q --detach "${SECOND}"
printf '# TASK 529: No change\n\n- ci_record: %s\n' "$(jq -c . <<<"${NO_CHANGE}")" >"${REPOSITORY}/docs/tasks/done/529-no-change.md"
printf -- '- [ ] 500 Unfinished -> tasks/500-unfinished.md\n' >>"${REPOSITORY}/docs/tasks.md"
check_run --completion 2 open_task_board
printf 'Task CI receipt passed: exit classes, exact run identity, grouped ancestry/deduplication, complete closure mappings, unpublished and no-change state\n'

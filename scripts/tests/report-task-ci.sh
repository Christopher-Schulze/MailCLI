#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'Usage: report-task-ci.sh FULL_40_HEX_TASK_COMMIT | --pending|--completion --run-task NNN\n' >&2
}

MODE=single
if [[ "$#" == 3 && ( "$1" == --pending || "$1" == --completion ) && "$2" == --run-task && "$3" =~ ^[0-9]{3}$ ]]; then
  MODE="$1"
  RUN_TASK="$3"
elif [[ "$#" == 1 && "$1" =~ ^[0-9a-f]{40}$ ]]; then
  TASK_COMMIT="$1"
else
  usage
  exit 64
fi

unknown() {
  printf 'ci_receipt=unknown\nci_reason=%s\n' "$1"
  exit 3
}

for REQUIRED_COMMAND in gh jq; do
  if ! command -v "${REQUIRED_COMMAND}" >/dev/null 2>&1; then
    printf 'ci_receipt=unavailable\nci_reason=missing_%s\n' "${REQUIRED_COMMAND}"
    exit 69
  fi
done

query_ci() (
TASK_COMMIT="$1"
CI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)}"
CI_REMOTE="$(git -C "${CI_ROOT}" remote get-url origin 2>/dev/null)" || unknown missing_origin
case "${CI_REMOTE}" in
  https://github.com/*) CI_REPOSITORY="${CI_REMOTE#https://github.com/}" ;;
  git@github.com:*) CI_REPOSITORY="${CI_REMOTE#git@github.com:}" ;;
  *) unknown unsupported_origin ;;
esac
CI_REPOSITORY="${CI_REPOSITORY%.git}"
[[ "${CI_REPOSITORY}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || unknown invalid_origin
printf 'ci_commit=%s\n' "${TASK_COMMIT}"
RUNS="$(gh run list --repo "${CI_REPOSITORY}" --commit "${TASK_COMMIT}" --workflow ci.yml \
  --event push --branch main --limit 20 \
  --json databaseId,headSha,status,conclusion,createdAt,url,event,headBranch,name,attempt,workflowDatabaseId)" ||
  unknown run_list_unavailable
RUN_COUNT="$(jq -er 'if type == "array" then length else error("run list is not an array") end' \
  <<<"${RUNS}" 2>/dev/null)" || unknown malformed_run_list
if [[ "${RUN_COUNT}" -eq 0 ]]; then
  unknown missing_run
fi
if [[ "${RUN_COUNT}" -ne 1 ]]; then
  unknown ambiguous_runs
fi
if ! jq -e --arg sha "${TASK_COMMIT}" '
  .[0] |
  .headSha == $sha and .event == "push" and .headBranch == "main" and
  .name == "ci" and (.databaseId | type == "number") and
  (.databaseId > 0) and (.databaseId | floor == .) and (.attempt | type == "number") and
  (.attempt > 0) and (.attempt | floor == .) and
  (.workflowDatabaseId | type == "number" and . > 0 and floor == .)
' <<<"${RUNS}" >/dev/null 2>&1; then
  unknown list_identity_mismatch
fi
RUN_ID="$(jq -er '.[0].databaseId' <<<"${RUNS}")"
LIST_ATTEMPT="$(jq -er '.[0].attempt' <<<"${RUNS}")"
WORKFLOW_ID="$(jq -er '.[0].workflowDatabaseId' <<<"${RUNS}")"

RUN="$(gh run view "${RUN_ID}" --repo "${CI_REPOSITORY}" \
  --json databaseId,headSha,status,conclusion,url,event,headBranch,name,attempt,workflowDatabaseId)" ||
  unknown run_view_unavailable
if ! jq -e --arg sha "${TASK_COMMIT}" --argjson id "${RUN_ID}" \
  --argjson min_attempt "${LIST_ATTEMPT}" --argjson workflow "${WORKFLOW_ID}" --arg repository "${CI_REPOSITORY}" '
  type == "object" and .databaseId == $id and .headSha == $sha and
  .event == "push" and .headBranch == "main" and .name == "ci" and
  .workflowDatabaseId == $workflow and (.attempt | type == "number") and
  (.attempt | floor == .) and .attempt >= $min_attempt and
  (.status | type == "string") and (.conclusion == null or
  (.conclusion | type == "string")) and (.url | type == "string") and
  (.url == ("https://github.com/" + $repository + "/actions/runs/" + ($id | tostring)) or
   .url == ("https://github.com/" + $repository + "/actions/runs/" + ($id | tostring) + "/attempts/" + (.attempt | tostring)))
' <<<"${RUN}" >/dev/null 2>&1; then
  unknown view_identity_mismatch
fi

RUN_STATUS="$(jq -r '.status' <<<"${RUN}")"
RUN_CONCLUSION="$(jq -r '.conclusion // ""' <<<"${RUN}")"
printf 'ci_repository=%s\nci_workflow_id=%s\n' "${CI_REPOSITORY}" "${WORKFLOW_ID}"
printf 'ci_run_id=%s\nci_run_attempt=%s\nci_run_sha=%s\nci_run_status=%s\nci_run_conclusion=%s\nci_run_url=%s\n' \
  "${RUN_ID}" "$(jq -r '.attempt' <<<"${RUN}")" "${TASK_COMMIT}" \
  "${RUN_STATUS}" "${RUN_CONCLUSION}" "$(jq -r '.url' <<<"${RUN}")"

case "${RUN_STATUS}" in
  completed)
    case "${RUN_CONCLUSION}" in
      success)
        printf 'ci_receipt=success\n'
        exit 0
        ;;
      failure | cancelled | timed_out | action_required | stale | startup_failure | neutral | skipped)
        printf 'ci_receipt=failed\n'
        exit 1
        ;;
      *) unknown incomplete_conclusion ;;
    esac
    ;;
  queued | in_progress | requested | waiting | pending)
    [[ -z "${RUN_CONCLUSION}" ]] || unknown inconsistent_status
    printf 'ci_receipt=pending\n'
    exit 2
    ;;
  *) unknown unsupported_status ;;
esac
)

report_run() {
  local ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)}"
  local WINDOW_FILE WINDOW BASE INITIAL_DONE FILE ID RECORD COMMIT GROUP
  local SUBJECT MEMBERS MEMBER STATUS OUTPUT GROUP_STATUS OPEN_COUNT
  local WINDOWS=() DETAILS=()
  shopt -s nullglob
  WINDOWS=("${ROOT}/docs/tasks/${RUN_TASK}-"*.md "${ROOT}/docs/tasks/done/${RUN_TASK}-"*.md)
  [[ "${#WINDOWS[@]}" == 1 && -f "${WINDOWS[0]}" && ! -L "${WINDOWS[0]}" ]] || unknown missing_run_window
  WINDOW_FILE="${WINDOWS[0]}"
  WINDOW="$(sed -n 's/^- ci_run_window: //p' "${WINDOW_FILE}")"
  jq -e 'def sha: type == "string" and test("^[0-9a-f]{40}$");
    def ids: type == "array" and all(.[]; type == "string" and test("^[0-9]{3}$")) and length == (unique | length);
    .schema == 1 and (.base_sha | sha) and (.started_utc | type == "string") and
    (.initial_done | ids) and (.initial_open | ids) and
    ((.initial_done - .initial_open) | length) == (.initial_done | length)
  ' <<<"${WINDOW}" >/dev/null 2>&1 || unknown malformed_run_window
  BASE="$(jq -r '.base_sha' <<<"${WINDOW}")"
  INITIAL_DONE="$(jq -c '.initial_done' <<<"${WINDOW}")"
  git -C "${ROOT}" merge-base --is-ancestor "${BASE}" HEAD || unknown unrelated_run_baseline
  TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-ci-report.XXXXXX")"
  trap 'rm -rf -- "${TEST_ROOT}"' EXIT
  : >"${TEST_ROOT}/records"
  DETAILS=("${ROOT}/docs/tasks/done/"[0-9][0-9][0-9]-*.md)
  for FILE in "${DETAILS[@]}"; do
    [[ -f "${FILE}" && ! -L "${FILE}" ]] || unknown unsafe_task_record
    ID="${FILE##*/}"; ID="${ID:0:3}"
    if jq -e --arg id "${ID}" 'index($id) != null' <<<"${INITIAL_DONE}" >/dev/null; then continue; fi
    RECORD="$(sed -n 's/^- ci_record: //p' "${FILE}")"
    jq -e --arg base "${BASE}" --arg id "${ID}" '
      def sha: type == "string" and test("^[0-9a-f]{40}$");
      .schema == 1 and .run_base == $base and .task_id == $id and (.closed_head | sha) and
      ((.product_commit == null and .group_head == null and .ci_state == "not_required" and
        .run_id == null and .attempt == null and .url == null) or
       ((.product_commit | sha) and
        ((.group_head == null and .ci_state == "unpublished" and .run_id == null and .attempt == null and .url == null) or
         ((.group_head | sha) and (.ci_state | IN("pending", "success", "failed", "unknown")) and
          (if .ci_state == "success" then
            (.run_id | type == "number" and . > 0 and floor == .) and
            (.attempt | type == "number" and . > 0 and floor == .) and
            (.url | type == "string" and startswith("https://github.com/"))
           else true end)))))
    ' <<<"${RECORD}" >/dev/null 2>&1 || unknown "missing_or_malformed_task_${ID}"
    if jq -e 'has("previous_failed_receipt")' <<<"${RECORD}" >/dev/null; then
      jq -e '.group_head != null and (.previous_failed_receipt |
        .state == "failed" and (.group_head | type == "string" and test("^[0-9a-f]{40}$")) and
        (.run_id | type == "number" and . > 0 and floor == .) and
        (.attempt | type == "number" and . > 0 and floor == .) and
        (.url | type == "string" and startswith("https://github.com/")) and
        (.remediation_task | type == "string" and test("^[0-9]{3}$")))' \
        <<<"${RECORD}" >/dev/null || unknown "invalid_retained_failure_${ID}"
    fi
    [[ "$(git -C "${ROOT}" rev-parse "$(jq -r '.closed_head' <<<"${RECORD}")^{commit}" 2>/dev/null)" == "$(jq -r '.closed_head' <<<"${RECORD}")" ]] || unknown "invalid_closed_head_${ID}"
    git -C "${ROOT}" merge-base --is-ancestor "${BASE}" "$(jq -r '.closed_head' <<<"${RECORD}")" || unknown "foreign_closure_${ID}"
    git -C "${ROOT}" merge-base --is-ancestor "$(jq -r '.closed_head' <<<"${RECORD}")" HEAD || unknown "unintegrated_closure_${ID}"
    COMMIT="$(jq -r '.product_commit // empty' <<<"${RECORD}")"
    GROUP="$(jq -r '.group_head // empty' <<<"${RECORD}")"
    if [[ -n "${COMMIT}" ]]; then
      git -C "${ROOT}" merge-base --is-ancestor "${BASE}" "${COMMIT}" || unknown "foreign_product_${ID}"
      [[ "${COMMIT}" != "${BASE}" ]] || unknown "baseline_product_${ID}"
      git -C "${ROOT}" merge-base --is-ancestor "${COMMIT}" "$(jq -r '.closed_head' <<<"${RECORD}")" || unknown "invalid_closure_${ID}"
      SUBJECT="$(git -C "${ROOT}" show -s --format=%s "${COMMIT}")"
      [[ "${SUBJECT}" =~ ^TASK\ ([0-9]{3}(,\ [0-9]{3})*):\  ]] || unknown "invalid_product_subject_${ID}"
      MEMBERS="${BASH_REMATCH[1]}"
      [[ ",${MEMBERS// /}," == *",${ID},"* ]] || unknown "wrong_product_member_${ID}"
    fi
    if [[ -n "${GROUP}" ]]; then
      git -C "${ROOT}" merge-base --is-ancestor "${COMMIT}" "${GROUP}" || unknown "group_ancestry_${ID}"
      git -C "${ROOT}" merge-base --is-ancestor "${GROUP}" HEAD || unknown "foreign_group_${ID}"
    fi
    printf '%s\n' "${RECORD}" >>"${TEST_ROOT}/records"
    printf 'ci_task=%s\nci_product_commit=%s\nci_group_head=%s\nci_recorded_state=%s\n' \
      "${ID}" "${COMMIT:-none}" "${GROUP:-none}" "$(jq -r '.ci_state' <<<"${RECORD}")"
  done
  jq -se 'map(.task_id) | length == (unique | length)' "${TEST_ROOT}/records" >/dev/null || unknown duplicate_task_records
  while IFS=$'\t' read -r ID MEMBER COMMIT GROUP; do
    local REPAIR
    REPAIR="$(jq -ser --arg id "${MEMBER}" --arg group "${GROUP}" \
      'map(select(.task_id == $id and .group_head == $group and .product_commit != null)) |
       if length == 1 then .[0].product_commit else error("missing repair owner") end' \
      "${TEST_ROOT}/records")" || unknown "missing_remediation_${ID}"
    [[ "${REPAIR}" != "${COMMIT}" ]] || unknown "nonrepair_group_${ID}"
    git -C "${ROOT}" merge-base --is-ancestor "${COMMIT}" "${REPAIR}" || unknown "unrelated_remediation_${ID}"
  done < <(jq -sr '.[] | select(has("previous_failed_receipt")) |
    [.task_id,.previous_failed_receipt.remediation_task,.previous_failed_receipt.group_head,.group_head] | @tsv' "${TEST_ROOT}/records")
  while IFS=$'\t' read -r COMMIT SUBJECT; do
    [[ "${SUBJECT}" =~ ^TASK\ ([0-9]{3}(,\ [0-9]{3})*):\  ]] || continue
    MEMBERS="${BASH_REMATCH[1]}"
    for MEMBER in ${MEMBERS//,/ }; do
      local ACCEPTED_PRODUCT
      ACCEPTED_PRODUCT="$(jq -ser --arg id "${MEMBER}" \
        'map(select(.task_id == $id and .product_commit != null)) |
         if length == 1 then .[0].product_commit else error("missing accepted product") end' \
        "${TEST_ROOT}/records")" || unknown "missing_product_mapping_${MEMBER}"
      git -C "${ROOT}" merge-base --is-ancestor "${COMMIT}" "${ACCEPTED_PRODUCT}" ||
        unknown "uncovered_product_commit_${MEMBER}"
    done
  done < <(git -C "${ROOT}" log --format='%H%x09%s' "${BASE}..HEAD")
  STATUS=0
  while IFS= read -r GROUP; do
    GROUP_STATUS=0
    OUTPUT="$(cd "${ROOT}" && query_ci "${GROUP}")" || GROUP_STATUS=$?
    printf '%s\n' "${OUTPUT}"
    case "${GROUP_STATUS}" in
      1) STATUS=1 ;;
      3) [[ "${STATUS}" == 1 ]] || STATUS=3 ;;
      2) [[ "${STATUS}" == 1 || "${STATUS}" == 3 ]] || STATUS=2 ;;
      0) ;;
      *) [[ "${STATUS}" == 1 ]] || STATUS=3 ;;
    esac
    if [[ "${MODE}" == --completion && "${GROUP_STATUS}" == 0 ]]; then
      local ACTUAL_ID ACTUAL_ATTEMPT ACTUAL_URL
      ACTUAL_ID="$(sed -n 's/^ci_run_id=//p' <<<"${OUTPUT}")"
      ACTUAL_ATTEMPT="$(sed -n 's/^ci_run_attempt=//p' <<<"${OUTPUT}")"
      ACTUAL_URL="$(sed -n 's/^ci_run_url=//p' <<<"${OUTPUT}")"
      if ! jq -se --arg group "${GROUP}" --arg id "${ACTUAL_ID}" --arg attempt "${ACTUAL_ATTEMPT}" --arg url "${ACTUAL_URL}" \
        'all(.[] | select(.group_head == $group); .ci_state == "success" and
          (.run_id | tostring) == $id and (.attempt | tostring) == $attempt and .url == $url)' \
        "${TEST_ROOT}/records" >/dev/null; then
        printf 'ci_reason=unpersisted_or_stale_group_receipt\n'
        [[ "${STATUS}" == 1 ]] || STATUS=3
      fi
    fi
  done < <(jq -sr 'map(.group_head // empty) | unique[]' "${TEST_ROOT}/records")
  if jq -se 'any(.[]; .ci_state == "unpublished")' "${TEST_ROOT}/records" >/dev/null; then
    printf 'ci_unpublished=true\n'
    [[ "${STATUS}" == 1 || "${STATUS}" == 3 ]] || STATUS=2
  fi
  if [[ "$(git -C "${ROOT}" rev-parse HEAD)" != "${BASE}" ]] &&
    ! jq -se --arg head "$(git -C "${ROOT}" rev-parse HEAD)" 'any(.[]; .group_head == $head)' "${TEST_ROOT}/records" >/dev/null; then
    printf 'ci_unpublished_head=true\n'
    [[ "${STATUS}" == 1 || "${STATUS}" == 3 ]] || STATUS=2
  fi
  if [[ "${MODE}" == --completion ]]; then
    if ! jq -se --argjson expected "$(jq -c '.initial_open' <<<"${WINDOW}")" \
      'map(.task_id) as $done | ($expected - $done) | length == 0' "${TEST_ROOT}/records" >/dev/null; then
      printf 'ci_reason=incomplete_run_closure_set\n'
      [[ "${STATUS}" == 1 || "${STATUS}" == 3 ]] || STATUS=2
    fi
    OPEN_COUNT="$(grep -Ec '^- \[[ ~!]\] [0-9]{3} ' "${ROOT}/docs/tasks.md" || true)"
    if [[ "${OPEN_COUNT}" != 0 ]]; then
      printf 'ci_reason=open_task_board\n'
      [[ "${STATUS}" == 1 || "${STATUS}" == 3 ]] || STATUS=2
    fi
  fi
  printf 'ci_run_base=%s\nci_report_status=%s\n' "${BASE}" "${STATUS}"
  return "${STATUS}"
}

if [[ "${MODE}" == single ]]; then
  query_ci "${TASK_COMMIT}"
else
  report_run
fi

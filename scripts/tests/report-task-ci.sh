#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'Usage: report-task-ci.sh FULL_40_HEX_TASK_COMMIT\n' >&2
}

if [[ "$#" -ne 1 || ! "$1" =~ ^[0-9a-f]{40}$ ]]; then
  usage
  exit 2
fi
TASK_COMMIT="$1"
printf 'ci_commit=%s\n' "${TASK_COMMIT}"

unknown() {
  printf 'ci_receipt=unknown\nci_reason=%s\n' "$1"
  exit 3
}

for REQUIRED_COMMAND in gh jq; do
  command -v "${REQUIRED_COMMAND}" >/dev/null 2>&1 ||
    unknown "missing_${REQUIRED_COMMAND}"
done

RUNS="$(gh run list --commit "${TASK_COMMIT}" --workflow ci.yml \
  --event push --branch main --limit 20 \
  --json databaseId,headSha,status,conclusion,createdAt,url,event,headBranch,name,attempt)" ||
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
  (.databaseId > 0) and (.attempt | type == "number") and
  (.attempt > 0)
' <<<"${RUNS}" >/dev/null 2>&1; then
  unknown list_identity_mismatch
fi
RUN_ID="$(jq -er '.[0].databaseId' <<<"${RUNS}")"
LIST_ATTEMPT="$(jq -er '.[0].attempt' <<<"${RUNS}")"

RUN="$(gh run view "${RUN_ID}" \
  --json databaseId,headSha,status,conclusion,url,event,headBranch,name,attempt)" ||
  unknown run_view_unavailable
if ! jq -e --arg sha "${TASK_COMMIT}" --argjson id "${RUN_ID}" \
  --argjson min_attempt "${LIST_ATTEMPT}" '
  type == "object" and .databaseId == $id and .headSha == $sha and
  .event == "push" and .headBranch == "main" and .name == "ci" and
  (.attempt | type == "number") and .attempt >= $min_attempt and
  (.status | type == "string") and (.conclusion == null or
  (.conclusion | type == "string")) and (.url | type == "string") and
  (.url | startswith("https://github.com/"))
' <<<"${RUN}" >/dev/null 2>&1; then
  unknown view_identity_mismatch
fi

RUN_STATUS="$(jq -r '.status' <<<"${RUN}")"
RUN_CONCLUSION="$(jq -r '.conclusion // ""' <<<"${RUN}")"
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

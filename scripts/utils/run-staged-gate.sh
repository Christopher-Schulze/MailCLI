#!/usr/bin/env bash
set -euo pipefail

# Executed from the lease baseline, never from the staged patch itself.
[[ "$#" -eq 4 || ( "$#" -eq 5 && ( "$5" == --fast || "$5" == --full ) ) || ( "$#" -ge 6 && "$5" == --checks ) ]] || {
  printf 'Usage: run-staged-gate.sh ROOT BASELINE INDEX_TREE TASK_IDS [--fast|--full|--checks REGISTERED_PATH... [--expect-baseline-failure PATH EXACT_FINAL_LINE]]\n' >&2; exit 2;
}
SOURCE_ROOT="$1"
BASELINE_HEAD="$2"
INDEX_TREE="$3"
TASK_IDS="$4"
shift 4
FAST_SHELL_ONLY=false
if [[ "${1:-}" == --fast ]]; then
  if git -C "${SOURCE_ROOT}" cat-file -e "${BASELINE_HEAD}:go.mod" 2>/dev/null ||
    git -C "${SOURCE_ROOT}" cat-file -e "${INDEX_TREE}:go.mod" 2>/dev/null; then
    GIT_DIRECTORY="$(git -C "${SOURCE_ROOT}" rev-parse --absolute-git-dir)"
    LINT_RECEIPT=""
    [[ ! -d "${GIT_DIRECTORY}/mailcli-write-lease" ]] || LINT_RECEIPT="${GIT_DIRECTORY}/mailcli-write-lease/lint_identity"
    exec "$(dirname "${BASH_SOURCE[0]}")/run-fast-gate.sh" "${SOURCE_ROOT}" "${BASELINE_HEAD}" "${INDEX_TREE}" --fast \
      "${LINT_RECEIPT}" "${TASK_IDS}"
  fi
  # A shell-only product has no Go analysis; its registered shell checks remain required.
  FAST_SHELL_ONLY=true
fi
GATE_TIER=full
SELECTED_CASES=()
EXPECTED_BASELINE_FAILURE_PATH=''
EXPECTED_BASELINE_DIAGNOSTIC=''
if [[ "${1:-}" == --expect-baseline-failure ]]; then
  printf 'Expected baseline failure is only valid after --checks\n' >&2
  exit 2
fi
if [[ "${1:-}" == --checks ]]; then
  GATE_TIER=targeted
  shift
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --expect-baseline-failure)
        [[ "$#" -eq 3 ]] || {
          printf 'Expected-baseline-failure option requires PATH and one exact final diagnostic line\n' >&2
          exit 2
        }
        EXPECTED_BASELINE_FAILURE_PATH="$2"
        EXPECTED_BASELINE_DIAGNOSTIC="$3"
        break
        ;;
      --*)
        printf 'Unknown targeted gate option: %s\n' "$1" >&2
        exit 2
        ;;
      *)
        SELECTED_CASES+=("$1")
        shift
        ;;
    esac
  done
  [[ "${#SELECTED_CASES[@]}" -gt 0 ]] || {
    printf 'Targeted gate requires at least one registered check\n' >&2
    exit 2
  }
fi
[[ "${FAST_SHELL_ONLY}" == false ]] || GATE_TIER=targeted
for VARIABLE in $(git rev-parse --local-env-vars); do unset "${VARIABLE}"; done
unset MAILCLI_WRITE_ROOT MAILCLI_GATE_RECEIPTS
umask 077
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-staged-gate.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
PRODUCT_ROOT="${TEST_ROOT}/product"
BASELINE_ROOT="${TEST_ROOT}/baseline"
RECEIPTS="${TEST_ROOT}/receipts"
: >"${RECEIPTS}"

fail() { printf '%s\n' "$1" >&2; exit 1; }
if [[ -n "${EXPECTED_BASELINE_FAILURE_PATH}" ]]; then
  [[ "${EXPECTED_BASELINE_FAILURE_PATH}" =~ ^scripts/tests/test-[a-z0-9-]+\.sh$ ]] ||
    fail 'Expected baseline failure path must be a canonical registered shell test'
  [[ -n "${EXPECTED_BASELINE_DIAGNOSTIC}" && "${EXPECTED_BASELINE_DIAGNOSTIC}" != *$'\n'* ]] ||
    fail 'Expected baseline diagnostic must be one non-empty line'
fi
git clone -q --shared --no-checkout --no-tags "${SOURCE_ROOT}" "${PRODUCT_ROOT}"
git -C "${PRODUCT_ROOT}" checkout -q --detach "${BASELINE_HEAD}"
git -C "${PRODUCT_ROOT}" read-tree --reset -u "${INDEX_TREE}"
cd "${PRODUCT_ROOT}"
PRODUCT_REFS_BEFORE="$(git -C "${PRODUCT_ROOT}" for-each-ref --format='%(refname) %(objectname)')"
mkdir "${BASELINE_ROOT}"
BASELINE_PATHS=(scripts/tests)
# Tests honoring MAILCLI_ROOT inspect the staged product. The summary test
# instead sources this support file relative to its own archived script path.
# Missing baseline support stays missing, even if another tree supplies it.
BASELINE_SUPPORT_FILES=(scripts/benchmarks/summarize-performance-evidence.sh)
for SUPPORT_PATH in "${BASELINE_SUPPORT_FILES[@]}"; do
  SUPPORT_ENTRY="$(git -C "${SOURCE_ROOT}" ls-tree "${BASELINE_HEAD}" -- "${SUPPORT_PATH}")"
  [[ -n "${SUPPORT_ENTRY}" ]] || continue
  read -r SUPPORT_MODE SUPPORT_TYPE SUPPORT_BLOB SUPPORT_NAME <<<"${SUPPORT_ENTRY}"
  [[ "${SUPPORT_TYPE}" == blob && "${SUPPORT_NAME}" == "${SUPPORT_PATH}" &&
    ( "${SUPPORT_MODE}" == 100644 || "${SUPPORT_MODE}" == 100755 ) ]] ||
    fail "Baseline support must be a regular tracked file: ${SUPPORT_PATH}"
  BASELINE_PATHS+=("${SUPPORT_PATH}")
done
git -C "${SOURCE_ROOT}" archive "${BASELINE_HEAD}" "${BASELINE_PATHS[@]}" |
  tar -x -C "${BASELINE_ROOT}"
for SUPPORT_PATH in "${BASELINE_PATHS[@]:1}"; do
  SUPPORT_BLOB="$(git -C "${SOURCE_ROOT}" rev-parse "${BASELINE_HEAD}:${SUPPORT_PATH}")"
  [[ -f "${BASELINE_ROOT}/${SUPPORT_PATH}" && ! -L "${BASELINE_ROOT}/${SUPPORT_PATH}" &&
    "$(git -C "${SOURCE_ROOT}" hash-object --no-filters "${BASELINE_ROOT}/${SUPPORT_PATH}")" == "${SUPPORT_BLOB}" ]] ||
    fail "Archived baseline support differs from its Git object: ${SUPPORT_PATH}"
  printf 'baseline_support_file=%s:%s:%s\n' "${BASELINE_HEAD}" "${SUPPORT_BLOB}" "${SUPPORT_PATH}"
done
HARNESS="${PRODUCT_ROOT}/scripts/tests/test.sh"
[[ -f "${HARNESS}" && ! -L "${HARNESS}" && -x "${HARNESS}" ]] ||
  fail 'Staged gate orchestrator must remain a regular executable file'
grep -Fxq '# MAILCLI_GATE_HARNESS=staged-v1' "${HARNESS}" ||
  fail 'Staged gate harness marker was removed'


read_manifest() {
  local ROOT="$1"
  local OPTION="$2"
  local DESTINATION="$3"
  MAILCLI_ROOT="${PRODUCT_ROOT}" "${ROOT}/scripts/tests/test.sh" "${OPTION}" >"${DESTINATION}"
  local CASE_PATH
  while IFS= read -r CASE_PATH; do
    [[ "${CASE_PATH}" =~ ^scripts/tests/test-[a-z0-9-]+\.sh$ ]] ||
      fail "Noncanonical shell test registration: ${CASE_PATH}"
    [[ -f "${ROOT}/${CASE_PATH}" && ! -L "${ROOT}/${CASE_PATH}" && -x "${ROOT}/${CASE_PATH}" ]] ||
      fail "Registered shell test is not a regular executable: ${CASE_PATH}"
  done <"${DESTINATION}"
  [[ -z "$(LC_ALL=C sort "${DESTINATION}" | uniq -d)" ]] ||
    fail 'Duplicate shell test registration'
}
grep -Fxq '# MAILCLI_GATE_HARNESS=staged-v1' "${BASELINE_ROOT}/scripts/tests/test.sh" ||
  fail 'Baseline gate harness marker is required'
read_manifest "${BASELINE_ROOT}" --list-shell-tests "${TEST_ROOT}/baseline-cases"
read_manifest "${BASELINE_ROOT}" --list-live-shell-tests "${TEST_ROOT}/baseline-live"
read_manifest "${PRODUCT_ROOT}" --list-shell-tests "${TEST_ROOT}/staged-cases"
read_manifest "${PRODUCT_ROOT}" --list-live-shell-tests "${TEST_ROOT}/staged-live"
cat "${TEST_ROOT}/staged-cases" "${TEST_ROOT}/staged-live" |
  LC_ALL=C sort >"${TEST_ROOT}/registered"
[[ -z "$(uniq -d "${TEST_ROOT}/registered")" ]] || fail 'Shell test registered in multiple modes'
git -C "${PRODUCT_ROOT}" ls-files scripts/tests |
  awk '/^scripts\/tests\/test-.*\.sh$/' |
  LC_ALL=C sort >"${TEST_ROOT}/inventory"
while IFS= read -r CASE_PATH; do
  [[ "${CASE_PATH}" =~ ^scripts/tests/test-[a-z0-9-]+\.sh$ ]] ||
    fail "Noncanonical shell test filename: ${CASE_PATH}"
done <"${TEST_ROOT}/inventory"
diff -u "${TEST_ROOT}/inventory" "${TEST_ROOT}/registered" ||
  fail 'Every staged shell test must be registered, including opt-in live cases'

retired_case() {
  local CASE_PATH="$1"
  # TASK 568 retires the unused worktree reservations and per-task CI report.
  case ",${TASK_IDS},:${CASE_PATH}" in
    *,568,*:scripts/tests/test-worktree-coordination.sh | *,568,*:scripts/tests/test-task-ci-report.sh)
      [[ ! -e "${PRODUCT_ROOT}/${CASE_PATH}" ]]
      return
      ;;
  esac
  case ",${TASK_IDS}," in *,509,*) ;; *) return 1 ;; esac
  case "${CASE_PATH}" in
    scripts/tests/test-private-closure.sh | scripts/tests/test-task-history-export.sh) ;;
    *) return 1 ;;
  esac
  [[ ! -e "${PRODUCT_ROOT}/${CASE_PATH}" &&
    ! -e "${PRODUCT_ROOT}/scripts/utils/export-task-history.sh" ]] || return 1
  ! grep -Eq '^(private_task_snapshot|verify_private_task_scope|source_task_manifest|private_proof_lease)\(\)|private-proof' \
    "${PRODUCT_ROOT}/scripts/utils/manage-write-lease.sh"
}

# Ordinary cases cannot silently become opt-in live cases; retained live cases
# must keep their explicit registration. Only the named obsolete feature retires.
while IFS= read -r CASE_PATH; do
  if ! grep -Fxq "${CASE_PATH}" "${TEST_ROOT}/staged-cases"; then
    retired_case "${CASE_PATH}" || fail "Baseline shell test removed or made opt-in: ${CASE_PATH}"
    printf 'retired_shell_test=%s\n' "${CASE_PATH}"
  fi
done <"${TEST_ROOT}/baseline-cases"
while IFS= read -r CASE_PATH; do
  grep -Fxq "${CASE_PATH}" "${TEST_ROOT}/staged-live" ||
    fail "Baseline live test registration removed: ${CASE_PATH}"
done <"${TEST_ROOT}/baseline-live"

cp "${TEST_ROOT}/staged-cases" "${TEST_ROOT}/executed-cases"
if [[ "${GATE_TIER}" == targeted ]]; then
  if [[ "${FAST_SHELL_ONLY}" == true ]]; then
    while IFS= read -r CASE_PATH; do SELECTED_CASES+=("${CASE_PATH}"); done <"${TEST_ROOT}/staged-cases"
  fi
  printf '%s\n' "${SELECTED_CASES[@]}" >"${TEST_ROOT}/executed-cases"
  [[ -z "$(LC_ALL=C sort "${TEST_ROOT}/executed-cases" | uniq -d)" ]] || fail 'Duplicate selected check'
  while IFS= read -r CASE_PATH; do
    grep -Fxq "${CASE_PATH}" "${TEST_ROOT}/staged-cases" || fail "Unregistered selected check: ${CASE_PATH}"
  done <"${TEST_ROOT}/executed-cases"
  if [[ -n "${EXPECTED_BASELINE_FAILURE_PATH}" ]]; then
    grep -Fxq "${EXPECTED_BASELINE_FAILURE_PATH}" "${TEST_ROOT}/baseline-cases" ||
      fail 'Expected baseline failure path is not registered at the lease baseline'
    grep -Fxq "${EXPECTED_BASELINE_FAILURE_PATH}" "${TEST_ROOT}/staged-cases" ||
      fail 'Expected baseline failure path is not registered in the staged suite'
    grep -Fxq "${EXPECTED_BASELINE_FAILURE_PATH}" "${TEST_ROOT}/executed-cases" ||
      fail 'Expected baseline failure path must also be selected for staged execution'
    BASELINE_TEST_HASH="$(shasum -a 256 "${BASELINE_ROOT}/${EXPECTED_BASELINE_FAILURE_PATH}" | awk '{print $1}')"
    STAGED_TEST_HASH="$(shasum -a 256 "${PRODUCT_ROOT}/${EXPECTED_BASELINE_FAILURE_PATH}" | awk '{print $1}')"
    [[ "${BASELINE_TEST_HASH}" != "${STAGED_TEST_HASH}" ]] ||
      fail 'Expected baseline failure requires a changed staged test at the same path'
  fi
  MAILCLI_ROOT="${PRODUCT_ROOT}" MAILCLI_GATE_RECEIPTS="${RECEIPTS}" "${HARNESS}" --checks "${SELECTED_CASES[@]}"
else
  MAILCLI_ROOT="${PRODUCT_ROOT}" MAILCLI_GATE_RECEIPTS="${RECEIPTS}" "${HARNESS}" --full-checks
fi
: >"${TEST_ROOT}/expected-receipts"
if [[ "${GATE_TIER}" == full ]]; then
  CORE_HASH="$(MAILCLI_ROOT="${PRODUCT_ROOT}" "${HARNESS}" --core-source | shasum -a 256 | awk '{print $1}')"
  printf '%s\tcore-checks\n' "${CORE_HASH}" >>"${TEST_ROOT}/expected-receipts"
fi
while IFS= read -r CASE_PATH; do
  CASE_HASH="$(shasum -a 256 "${PRODUCT_ROOT}/${CASE_PATH}" | awk '{print $1}')"
  printf '%s\t%s\n' "${CASE_HASH}" "${CASE_PATH}" >>"${TEST_ROOT}/expected-receipts"
done <"${TEST_ROOT}/executed-cases"
LC_ALL=C sort "${RECEIPTS}" >"${TEST_ROOT}/actual-receipts"
LC_ALL=C sort "${TEST_ROOT}/expected-receipts" >"${TEST_ROOT}/sorted-expected"
diff -u "${TEST_ROOT}/sorted-expected" "${TEST_ROOT}/actual-receipts" ||
  fail 'Staged harness did not execute every registered shell test exactly once'

HARNESS_MODE=staged
if [[ "${GATE_TIER}" == full ]]; then
  BASELINE_CORE_HASH="$(MAILCLI_ROOT="${PRODUCT_ROOT}" "${BASELINE_ROOT}/scripts/tests/test.sh" --core-source | shasum -a 256 | awk '{print $1}')"
fi
if [[ "${GATE_TIER}" == full && "${BASELINE_CORE_HASH}" != "${CORE_HASH}" ]]; then
  MAILCLI_ROOT="${PRODUCT_ROOT}" "${BASELINE_ROOT}/scripts/tests/test.sh" --core-only
  HARNESS_MODE=staged+baseline
fi
while IFS= read -r CASE_PATH; do
  retired_case "${CASE_PATH}" && continue
  if [[ "${GATE_TIER}" == targeted ]] && ! grep -Fxq "${CASE_PATH}" "${TEST_ROOT}/executed-cases"; then continue; fi
  BASELINE_HASH="$(shasum -a 256 "${BASELINE_ROOT}/${CASE_PATH}" | awk '{print $1}')"
  if ! grep -Fxq "${BASELINE_HASH}"$'\t'"${CASE_PATH}" "${RECEIPTS}"; then
    printf 'baseline_shell_test=%s\n' "${CASE_PATH}"
    if [[ "${CASE_PATH}" != "${EXPECTED_BASELINE_FAILURE_PATH}" ]]; then
      MAILCLI_ROOT="${PRODUCT_ROOT}" "${BASELINE_ROOT}/${CASE_PATH}"
      HARNESS_MODE=staged+baseline
      continue
    fi
    BASELINE_STATUS=0
    if BASELINE_OUTPUT="$(MAILCLI_ROOT="${PRODUCT_ROOT}" "${BASELINE_ROOT}/${CASE_PATH}" 2>&1)"; then
      BASELINE_STATUS=0
    else
      BASELINE_STATUS=$?
    fi
    if [[ "${BASELINE_STATUS}" -eq 0 ]]; then
      fail "Declared baseline failure did not occur: ${CASE_PATH}"
    fi
    if [[ "${BASELINE_STATUS}" -ne 1 ]]; then
      printf '%s\n' "${BASELINE_OUTPUT}" >&2
      fail "Expected baseline test to exit 1, got ${BASELINE_STATUS}: ${CASE_PATH}"
    fi
    BASELINE_LAST_LINE="$(printf '%s\n' "${BASELINE_OUTPUT}" | awk 'NF { line=$0 } END { print line }')"
    [[ "${BASELINE_LAST_LINE}" == "${EXPECTED_BASELINE_DIAGNOSTIC}" ]] || {
      printf '%s\n' "${BASELINE_OUTPUT}" >&2
      fail "Baseline shell test failure did not match the exact expected final diagnostic: ${CASE_PATH}"
    }
    printf 'baseline_expected_failure=%s\nbaseline_expected_diagnostic=%s\n' \
      "${CASE_PATH}" "${BASELINE_LAST_LINE}"
    HARNESS_MODE=staged+baseline
  fi
done <"${TEST_ROOT}/baseline-cases"
[[ "$(git -C "${PRODUCT_ROOT}" rev-parse HEAD)" == "${BASELINE_HEAD}" &&
  "$(git -C "${PRODUCT_ROOT}" write-tree)" == "${INDEX_TREE}" ]] ||
  fail 'Isolated gate HEAD or index changed during verification'
git -C "${PRODUCT_ROOT}" diff --quiet || fail 'Isolated gate product bytes changed during verification'
[[ -z "$(git -C "${PRODUCT_ROOT}" ls-files -v | grep -E '^[a-zS] ' || true)" ]] ||
  fail 'Isolated gate changed tracked index verification flags'
[[ -z "$(git -C "${PRODUCT_ROOT}" ls-files --others --exclude-standard)" ]] ||
  fail 'Isolated gate left nonignored product files'
[[ "$(git -C "${PRODUCT_ROOT}" for-each-ref --format='%(refname) %(objectname)')" == "${PRODUCT_REFS_BEFORE}" ]] ||
  fail 'Isolated gate changed branch, remote-tracking or tag refs'
printf 'gate_baseline_head=%s\ngate_harness=%s\ngate_index_tree=%s\n' \
  "${BASELINE_HEAD}" "${HARNESS_MODE}" "${INDEX_TREE}"
printf 'gate_tier=%s\n' "${GATE_TIER}"
sed 's/^/shell_test_receipt=/' "${TEST_ROOT}/actual-receipts"

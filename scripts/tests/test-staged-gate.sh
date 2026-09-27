#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
GATE_TOOL="${MAILCLI_ROOT}/scripts/utils/run-staged-gate.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-staged-gate-test.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
REPOSITORY="${TEST_ROOT}/repo"
mkdir -p "${REPOSITORY}/scripts/tests" "${REPOSITORY}/scripts/utils"
git -C "${REPOSITORY}" init -q -b main
git -C "${REPOSITORY}" config user.name 'MailCLI Test'
git -C "${REPOSITORY}" config user.email 'mailcli-test@example.invalid'
cp "${MAILCLI_ROOT}/scripts/utils/manage-write-lease.sh" "${REPOSITORY}/scripts/utils/manage-write-lease.sh"
cp "${GATE_TOOL}" "${REPOSITORY}/scripts/utils/run-staged-gate.sh"
printf '/docs/tasks.md\n/docs/tasks/\n/ignored-output\n' >"${REPOSITORY}/.gitignore"
printf 'required\n' >"${REPOSITORY}/product.txt"
printf 'obsolete\n' >"${REPOSITORY}/deleted.txt"
printf 'private-proof\n' >"${REPOSITORY}/scripts/utils/export-task-history.sh"
mkdir -p "${REPOSITORY}/docs/tasks/done"
printf '# MailCLI Tasks\n' >"${REPOSITORY}/docs/tasks.md"
printf '# TASK 491: Fixture\n' >"${REPOSITORY}/docs/tasks/491-fixture.md"

cat >"${REPOSITORY}/scripts/tests/test.sh" <<'FIXTURE'
#!/usr/bin/env bash
# MAILCLI_GATE_HARNESS=staged-v1
set -euo pipefail
ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
run_core() {
  set -euo pipefail
  test -f "${ROOT}/product.txt"
  if [[ -n "${MAILCLI_GATE_RECEIPTS:-}" ]]; then
    CORE_HASH="$(declare -f run_core | shasum -a 256 | awk '{print $1}')"
    printf '%s\tcore-checks\n' "${CORE_HASH}" >>"${MAILCLI_GATE_RECEIPTS}"
  fi
}
case "${1:-}" in
  --list-shell-tests) cat "${SCRIPT_ROOT}/scripts/tests/cases"; exit 0 ;;
  --list-live-shell-tests) cat "${SCRIPT_ROOT}/scripts/tests/live-cases"; exit 0 ;;
  --core-source) declare -f run_core; exit 0 ;;
  --core-only) unset MAILCLI_GATE_RECEIPTS; run_core; exit 0 ;;
  --checks)
    shift
    for CASE_PATH in "$@"; do
      "${SCRIPT_ROOT}/${CASE_PATH}" || exit "$?"
      HASH="$(shasum -a 256 "${SCRIPT_ROOT}/${CASE_PATH}" | awk '{print $1}')"
      printf '%s\t%s\n' "${HASH}" "${CASE_PATH}" >>"${MAILCLI_GATE_RECEIPTS}"
    done
    exit 0 ;;
esac
if [[ ! -f "${SCRIPT_ROOT}/skip-core" ]]; then run_core; fi
while IFS= read -r CASE_PATH; do
  if [[ -f "${SCRIPT_ROOT}/skip-case" && "${CASE_PATH}" == scripts/tests/test-invariant.sh ]]; then continue; fi
  "${SCRIPT_ROOT}/${CASE_PATH}"
  HASH="$(shasum -a 256 "${SCRIPT_ROOT}/${CASE_PATH}" | awk '{print $1}')"
  printf '%s\t%s\n' "${HASH}" "${CASE_PATH}" >>"${MAILCLI_GATE_RECEIPTS}"
done <"${SCRIPT_ROOT}/scripts/tests/cases"
FIXTURE
cat >"${REPOSITORY}/scripts/tests/test-invariant.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$(cat "${MAILCLI_ROOT}/product.txt")" != required ]]; then
  printf 'legacy product invariant no longer holds\n'
  exit 1
fi
printf 'invariant-ran\n'
FIXTURE
for CASE_PATH in test-private-closure.sh test-task-history-export.sh test-live-fixture.sh; do
  printf '#!/usr/bin/env bash\nexit 0\n' >"${REPOSITORY}/scripts/tests/${CASE_PATH}"
done
printf '%s\n' scripts/tests/test-invariant.sh scripts/tests/test-private-closure.sh \
  scripts/tests/test-task-history-export.sh >"${REPOSITORY}/scripts/tests/cases"
printf '%s\n' scripts/tests/test-live-fixture.sh >"${REPOSITORY}/scripts/tests/live-cases"
chmod 755 "${REPOSITORY}/scripts/tests/"*.sh "${REPOSITORY}/scripts/utils/"*.sh

stage_all() {
  local PATH_NAME
  local MODE
  local BLOB
  while IFS= read -r -d '' PATH_NAME; do
    MODE=100644
    [[ -x "${REPOSITORY}/${PATH_NAME}" ]] && MODE=100755
    BLOB="$(git -C "${REPOSITORY}" hash-object -w "${REPOSITORY}/${PATH_NAME}")"
    git -C "${REPOSITORY}" update-index --add --cacheinfo "${MODE},${BLOB},${PATH_NAME}"
  done < <(git -C "${REPOSITORY}" ls-files --cached --others --exclude-standard -z)
}
stage_all
BASELINE_TREE="$(git -C "${REPOSITORY}" write-tree)"
BASELINE_HEAD="$(printf 'staged gate fixture\n' | git -C "${REPOSITORY}" commit-tree "${BASELINE_TREE}")"
git -C "${REPOSITORY}" checkout -q --detach "${BASELINE_HEAD}"

reset_fixture() {
  # Only this test-owned throwaway repository is reset between independent cases.
  git -C "${REPOSITORY}" read-tree --reset -u "${BASELINE_TREE}"
  while IFS= read -r -d '' PATH_NAME; do rm -f "${REPOSITORY}/${PATH_NAME}"; done \
    < <(git -C "${REPOSITORY}" ls-files --others --exclude-standard -z)
}
run_gate() {
  local EXPECTED_STATUS="$1"
  local NEEDLE="$2"
  local TASK_IDS="${3:-491}"
  local STATUS=0
  local SOURCE_INDEX
  local SOURCE_HEAD
  local SOURCE_DIFF
  local SOURCE_REFS
  SOURCE_INDEX="$(git -C "${REPOSITORY}" write-tree)"
  SOURCE_HEAD="$(git -C "${REPOSITORY}" rev-parse HEAD)"
  SOURCE_DIFF="$(git -C "${REPOSITORY}" diff --binary)"
  SOURCE_REFS="$(git -C "${REPOSITORY}" for-each-ref --format='%(refname) %(objectname)')"
  local GATE_ARGS=(--full)
  [[ "$#" -le 3 ]] || GATE_ARGS=("${@:4}")
  "${GATE_TOOL}" "${REPOSITORY}" "${BASELINE_HEAD}" "${SOURCE_INDEX}" "${TASK_IDS}" "${GATE_ARGS[@]}" \
    >"${TEST_ROOT}/output" 2>&1 || STATUS=$?
  [[ "${STATUS}" == "${EXPECTED_STATUS}" ]] || {
    printf 'Staged fixture expected status %s, got %s\n' "${EXPECTED_STATUS}" "${STATUS}" >&2
    cat "${TEST_ROOT}/output" >&2; exit 1
  }
  [[ -z "${NEEDLE}" ]] || grep -Fq "${NEEDLE}" "${TEST_ROOT}/output" || {
    cat "${TEST_ROOT}/output" >&2; exit 1
  }
  [[ "$(git -C "${REPOSITORY}" write-tree)" == "${SOURCE_INDEX}" &&
    "$(git -C "${REPOSITORY}" rev-parse HEAD)" == "${SOURCE_HEAD}" &&
    "$(git -C "${REPOSITORY}" diff --binary)" == "${SOURCE_DIFF}" &&
    "$(git -C "${REPOSITORY}" for-each-ref --format='%(refname) %(objectname)')" == "${SOURCE_REFS}" ]]
}

run_gate 0 gate_harness=staged
[[ "$(grep -c '^invariant-ran$' "${TEST_ROOT}/output")" == 1 ]]
run_gate 0 gate_tier=targeted 491 --checks scripts/tests/test-invariant.sh
[[ "$(grep -c '^invariant-ran$' "${TEST_ROOT}/output")" == 1 ]]
! grep -Fq core-checks "${TEST_ROOT}/output"
run_gate 1 'Duplicate selected check' 491 --checks scripts/tests/test-invariant.sh scripts/tests/test-invariant.sh
run_gate 1 'Unregistered selected check' 491 --checks scripts/tests/test-missing.sh
sed '/^# MAILCLI_GATE_HARNESS=/d' "${REPOSITORY}/scripts/tests/test.sh" >"${TEST_ROOT}/markerless-orchestrator"
MARKERLESS_BLOB="$(git -C "${REPOSITORY}" hash-object -w "${TEST_ROOT}/markerless-orchestrator")"
git -C "${REPOSITORY}" update-index --cacheinfo "100755,${MARKERLESS_BLOB},scripts/tests/test.sh"
MARKERLESS_TREE="$(git -C "${REPOSITORY}" write-tree)"
MARKERLESS_BASELINE="$(printf 'unsupported markerless baseline\n' |
  git -C "${REPOSITORY}" commit-tree "${MARKERLESS_TREE}")"
git -C "${REPOSITORY}" read-tree --reset -u "${BASELINE_TREE}"
SUPPORTED_BASELINE="${BASELINE_HEAD}"
BASELINE_HEAD="${MARKERLESS_BASELINE}"
run_gate 1 'Baseline gate harness marker is required' 491 --checks scripts/tests/test-invariant.sh
BASELINE_HEAD="${SUPPORTED_BASELINE}"
printf 'skip\n' >"${REPOSITORY}/skip-core"
stage_all
run_gate 1 'did not execute every registered shell test'
reset_fixture
sed '/^# MAILCLI_GATE_HARNESS=/d' "${REPOSITORY}/scripts/tests/test.sh" >"${TEST_ROOT}/orchestrator"
cp "${TEST_ROOT}/orchestrator" "${REPOSITORY}/scripts/tests/test.sh"
stage_all
run_gate 1 'Staged gate harness marker was removed'
reset_fixture
printf 'scripts/tests/test-invariant.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
stage_all
run_gate 1 'Duplicate shell test registration'
reset_fixture

# Index-derived new/deleted paths and deliberate unstaged divergence.
cat >"${REPOSITORY}/scripts/tests/test-isolation.sh" <<FIXTURE
#!/usr/bin/env bash
set -euo pipefail
[[ "\$(cat "\${MAILCLI_ROOT}/indexed.txt")" == staged ]]
[[ ! -e "\${MAILCLI_ROOT}/deleted.txt" ]]
[[ "\$(git -C "\${MAILCLI_ROOT}" rev-parse --show-toplevel)" != '${REPOSITORY}' ]]
[[ "\$(git -C "\${MAILCLI_ROOT}" rev-parse --absolute-git-dir)" != '${REPOSITORY}/.git' ]]
[[ "\$(git -C "\${MAILCLI_ROOT}" rev-parse HEAD)" == '${BASELINE_HEAD}' ]]
! git -C "\${MAILCLI_ROOT}" diff --cached --quiet
FIXTURE
chmod 755 "${REPOSITORY}/scripts/tests/test-isolation.sh"
printf 'scripts/tests/test-isolation.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
printf 'staged\n' >"${REPOSITORY}/indexed.txt"
stage_all
git -C "${REPOSITORY}" update-index --force-remove deleted.txt
printf 'unstaged\n' >"${REPOSITORY}/indexed.txt"
run_gate 0 gate_harness=staged
reset_fixture

printf '#!/usr/bin/env bash\nexit 23\n' >"${REPOSITORY}/scripts/tests/test-new.sh"
chmod 755 "${REPOSITORY}/scripts/tests/test-new.sh"
stage_all
run_gate 1 'Every staged shell test must be registered'
printf 'scripts/tests/test-new.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
stage_all
run_gate 23 ''
run_gate 23 '' 491 --checks scripts/tests/test-new.sh
reset_fixture
printf '#!/usr/bin/env bash\nexit 23\n' >"${REPOSITORY}/scripts/tests/test-Bad.sh"
chmod 755 "${REPOSITORY}/scripts/tests/test-Bad.sh"
stage_all
run_gate 1 'Noncanonical shell test filename'
reset_fixture

printf 'skip\n' >"${REPOSITORY}/skip-case"
stage_all
run_gate 1 'did not execute every registered shell test'
reset_fixture
grep -v 'test-invariant.sh' "${REPOSITORY}/scripts/tests/cases" >"${TEST_ROOT}/cases"
cp "${TEST_ROOT}/cases" "${REPOSITORY}/scripts/tests/cases"
git -C "${REPOSITORY}" update-index --force-remove scripts/tests/test-invariant.sh
stage_all
# stage_all would re-add a present deleted file; remove only this owned fixture.
rm "${REPOSITORY}/scripts/tests/test-invariant.sh"
git -C "${REPOSITORY}" update-index --force-remove scripts/tests/test-invariant.sh
run_gate 1 'Baseline shell test removed or made opt-in'
reset_fixture
grep -v 'test-invariant.sh' "${REPOSITORY}/scripts/tests/cases" >"${TEST_ROOT}/cases"
cp "${TEST_ROOT}/cases" "${REPOSITORY}/scripts/tests/cases"
printf 'scripts/tests/test-invariant.sh\n' >>"${REPOSITORY}/scripts/tests/live-cases"
stage_all
run_gate 1 'Baseline shell test removed or made opt-in'
reset_fixture
: >"${REPOSITORY}/scripts/tests/live-cases"
rm "${REPOSITORY}/scripts/tests/test-live-fixture.sh"
git -C "${REPOSITORY}" update-index --force-remove scripts/tests/test-live-fixture.sh
stage_all
run_gate 1 'Baseline live test registration removed'
reset_fixture

printf '#!/usr/bin/env bash\nprintf "weakened\\n"\n' >"${REPOSITORY}/scripts/tests/test-invariant.sh"
printf 'broken\n' >"${REPOSITORY}/product.txt"
stage_all
run_gate 1 baseline_shell_test=scripts/tests/test-invariant.sh
reset_fixture
printf 'staged\n' >"${REPOSITORY}/product.txt"
cat >"${REPOSITORY}/scripts/tests/test-invariant.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(cat "${MAILCLI_ROOT}/product.txt")" == staged ]]
printf 'staged invariant passed\n'
FIXTURE
stage_all
run_gate 1 baseline_shell_test=scripts/tests/test-invariant.sh 491 --checks scripts/tests/test-invariant.sh
run_gate 0 baseline_expected_failure=scripts/tests/test-invariant.sh 491 --checks scripts/tests/test-invariant.sh \
  --expect-baseline-failure scripts/tests/test-invariant.sh 'legacy product invariant no longer holds'
grep -Fq 'baseline_expected_diagnostic=legacy product invariant no longer holds' "${TEST_ROOT}/output"
run_gate 1 'Baseline shell test failure did not match the exact expected final diagnostic' 491 \
  --checks scripts/tests/test-invariant.sh --expect-baseline-failure scripts/tests/test-invariant.sh 'different diagnostic'
printf '#!/usr/bin/env bash\nexit 23\n' >"${REPOSITORY}/scripts/tests/test-invariant.sh"
stage_all
run_gate 23 '' 491 --checks scripts/tests/test-invariant.sh \
  --expect-baseline-failure scripts/tests/test-invariant.sh 'legacy product invariant no longer holds'
reset_fixture
printf '\nprintf "changed invariant\\n"\n' >>"${REPOSITORY}/scripts/tests/test-invariant.sh"
stage_all
run_gate 1 'Declared baseline failure did not occur: scripts/tests/test-invariant.sh' 491 \
  --checks scripts/tests/test-invariant.sh --expect-baseline-failure scripts/tests/test-invariant.sh 'legacy product invariant no longer holds'
run_gate 0 gate_harness=staged+baseline
[[ "$(grep -c '^invariant-ran$' "${TEST_ROOT}/output")" == 2 ]]
reset_fixture

for MUTATION in tracked index untracked flags refs; do
  case "${MUTATION}" in
    tracked) ACTION='printf changed >"${MAILCLI_ROOT}/product.txt"' ;;
    index) ACTION='git -C "${MAILCLI_ROOT}" read-tree --empty' ;;
    untracked) ACTION='printf extra >"${MAILCLI_ROOT}/unexpected"' ;;
    flags) ACTION='git -C "${MAILCLI_ROOT}" update-index --assume-unchanged product.txt; printf changed >"${MAILCLI_ROOT}/product.txt"' ;;
    refs) ACTION='git -C "${MAILCLI_ROOT}" branch unexpected-ref' ;;
  esac
  printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "${ACTION}" \
    >"${REPOSITORY}/scripts/tests/test-mutation.sh"
  chmod 755 "${REPOSITORY}/scripts/tests/test-mutation.sh"
  printf 'scripts/tests/test-mutation.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
  stage_all
  run_gate 1 'Isolated gate'
  reset_fixture
done
printf '%s\n' '#!/usr/bin/env bash' \
  'printf output >"${MAILCLI_ROOT}/ignored-output"' \
  >"${REPOSITORY}/scripts/tests/test-output.sh"
chmod 755 "${REPOSITORY}/scripts/tests/test-output.sh"
printf 'scripts/tests/test-output.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
stage_all
run_gate 0 gate_harness=staged
reset_fixture

grep -v -e test-private-closure.sh -e test-task-history-export.sh \
  "${REPOSITORY}/scripts/tests/cases" >"${TEST_ROOT}/cases"
cp "${TEST_ROOT}/cases" "${REPOSITORY}/scripts/tests/cases"
for PATH_NAME in scripts/tests/test-private-closure.sh scripts/tests/test-task-history-export.sh \
  scripts/utils/export-task-history.sh; do
  rm "${REPOSITORY}/${PATH_NAME}"
  git -C "${REPOSITORY}" update-index --force-remove "${PATH_NAME}"
done
# Derive retirement from the real lease source; no unrelated case may retire.
sed '/private-proof/d; /^private_task_snapshot()/,/^}/d; /^verify_private_task_scope()/,/^}/d; /^source_task_manifest()/,/^}/d; /^private_proof_lease()/,/^}/d' \
  "${REPOSITORY}/scripts/utils/manage-write-lease.sh" >"${TEST_ROOT}/retired-tool"
cp "${TEST_ROOT}/retired-tool" "${REPOSITORY}/scripts/utils/manage-write-lease.sh"
stage_all
run_gate 1 'Baseline shell test removed or made opt-in'
run_gate 0 gate_harness=staged 509
rm "${REPOSITORY}/scripts/tests/test-invariant.sh"
git -C "${REPOSITORY}" update-index --force-remove scripts/tests/test-invariant.sh
grep -v test-invariant.sh "${REPOSITORY}/scripts/tests/cases" >"${TEST_ROOT}/cases" || [[ "$?" == 1 ]]
cp "${TEST_ROOT}/cases" "${REPOSITORY}/scripts/tests/cases"
stage_all
run_gate 1 'Baseline shell test removed or made opt-in' 509
reset_fixture

# Archived support must use baseline bytes, while assertions still inspect the
# isolated staged product. Deliberately divergent index/worktree helpers prove
# that neither tree can substitute for the baseline-owned dependency.
ORIGINAL_BASELINE_HEAD="${BASELINE_HEAD}"
ORIGINAL_BASELINE_TREE="${BASELINE_TREE}"
mkdir -p "${REPOSITORY}/scripts/benchmarks"
cat >"${REPOSITORY}/scripts/benchmarks/summarize-performance-evidence.sh" <<'FIXTURE'
baseline_support_identity() { printf 'baseline\n'; }
FIXTURE
cat >"${REPOSITORY}/scripts/tests/test-baseline-support.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${SCRIPT_ROOT}/scripts/benchmarks/summarize-performance-evidence.sh"
[[ "$(baseline_support_identity)" == baseline ]]
[[ "$(cat "${MAILCLI_ROOT}/product.txt")" == required ]] || {
  printf 'baseline support product invariant violated\n' >&2
  exit 1
}
printf 'baseline-support-ran\n'
FIXTURE
chmod 755 "${REPOSITORY}/scripts/tests/test-baseline-support.sh"
printf 'scripts/tests/test-baseline-support.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
stage_all
BASELINE_TREE="$(git -C "${REPOSITORY}" write-tree)"
BASELINE_HEAD="$(printf 'external baseline support fixture\n' |
  git -C "${REPOSITORY}" commit-tree "${BASELINE_TREE}")"
printf 'baseline_support_identity() { printf "staged\\n"; }\n' \
  >"${REPOSITORY}/scripts/benchmarks/summarize-performance-evidence.sh"
sed 's/== baseline/== staged/' "${REPOSITORY}/scripts/tests/test-baseline-support.sh" \
  >"${TEST_ROOT}/staged-support-test"
cp "${TEST_ROOT}/staged-support-test" "${REPOSITORY}/scripts/tests/test-baseline-support.sh"
stage_all
printf 'baseline_support_identity() { printf "worktree\\n"; }\n' \
  >"${REPOSITORY}/scripts/benchmarks/summarize-performance-evidence.sh"
run_gate 0 gate_harness=staged+baseline 544 --checks scripts/tests/test-baseline-support.sh
grep -Fq "baseline_support_file=${BASELINE_HEAD}:" "${TEST_ROOT}/output"
[[ "$(grep -c '^baseline-support-ran$' "${TEST_ROOT}/output")" == 2 ]]
printf 'broken\n' >"${REPOSITORY}/product.txt"
printf '#!/usr/bin/env bash\nexit 0\n' >"${REPOSITORY}/scripts/tests/test-baseline-support.sh"
stage_all
run_gate 1 'baseline support product invariant violated' 544 --checks scripts/tests/test-baseline-support.sh
reset_fixture

git -C "${REPOSITORY}" update-index --force-remove scripts/benchmarks/summarize-performance-evidence.sh
MISSING_SUPPORT_TREE="$(git -C "${REPOSITORY}" write-tree)"
BASELINE_HEAD="$(printf 'baseline missing external support\n' |
  git -C "${REPOSITORY}" commit-tree "${MISSING_SUPPORT_TREE}")"
# A staged-only dependency cannot repair an absent baseline dependency.
printf '\nprintf "changed support test\\n"\n' >>"${REPOSITORY}/scripts/tests/test-baseline-support.sh"
stage_all
run_gate 1 'No such file or directory' 544 --checks scripts/tests/test-baseline-support.sh
# Nor can an unstaged worktree-only dependency repair it, even when the staged
# test is weakened enough to finish without loading any helper.
printf '#!/usr/bin/env bash\nexit 0\n' >"${REPOSITORY}/scripts/tests/test-baseline-support.sh"
stage_all
git -C "${REPOSITORY}" update-index --force-remove scripts/benchmarks/summarize-performance-evidence.sh
run_gate 1 'No such file or directory' 544 --checks scripts/tests/test-baseline-support.sh
BASELINE_HEAD="${ORIGINAL_BASELINE_HEAD}"
BASELINE_TREE="${ORIGINAL_BASELINE_TREE}"
reset_fixture

# Outer lease integration uses the committed marker/helper in this independent
# repository: a staged newly failing test must not produce commit evidence.
ACQUIRE="$(MAILCLI_WRITE_ROOT="${REPOSITORY}" \
  "${REPOSITORY}/scripts/utils/manage-write-lease.sh" acquire 491 fixture-owner \
  scripts/tests/test-new.sh scripts/tests/cases)"
TOKEN="$(printf '%s\n' "${ACQUIRE}" | sed -n 's/^write_lease_token=//p')"
printf '#!/usr/bin/env bash\nexit 23\n' >"${REPOSITORY}/scripts/tests/test-new.sh"
chmod 755 "${REPOSITORY}/scripts/tests/test-new.sh"
printf 'scripts/tests/test-new.sh\n' >>"${REPOSITORY}/scripts/tests/cases"
stage_all
MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" review "${TOKEN}" >/dev/null
STATUS=0
MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" gate "${TOKEN}" --checks scripts/tests/test-new.sh \
  >"${TEST_ROOT}/lease-output" 2>&1 || STATUS=$?
[[ "${STATUS}" == 23 && ! -e "${REPOSITORY}/.git/mailcli-write-lease/gate_patch_sha256" ]]
reset_fixture
MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" abort "${TOKEN}" >/dev/null
printf 'Staged gate passed: isolated index product, exact baseline failures, baseline invariants, narrow retirement, mutation refusal, and failed own-gate proof\n'

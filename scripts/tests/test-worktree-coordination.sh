#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-worktree-coordination.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
PRIMARY="${TEST_ROOT}/primary"
TOOL="${SOURCE_ROOT}/scripts/utils/manage-write-lease.sh"
mkdir -p "${PRIMARY}/scripts/utils" "${PRIMARY}/scripts/tests" "${PRIMARY}/docs/tasks"
git -C "${PRIMARY}" init -q -b main
git -C "${PRIMARY}" config user.name 'MailCLI Test'
git -C "${PRIMARY}" config user.email 'mailcli-test@example.invalid'
git -C "${PRIMARY}" config core.ignorecase true
printf '/AGENTS.local.md\n/docs/tasks.md\n/docs/tasks/\n/ignored/\n' >"${PRIMARY}/.git/info/exclude"
printf 'Worker instructions: canonical board belongs to primary.\n' >"${PRIMARY}/AGENTS.local.md"
printf '# Tasks\n## Queue\n- [ ] 174 First -> tasks/174-first.md\n- [ ] 175 Second -> tasks/175-second.md\n- [ ] 176 Third -> tasks/176-third.md\n' >"${PRIMARY}/docs/tasks.md"
for ID in 174 175 176; do
  case "${ID}" in 174) SLUG=first ;; 175) SLUG=second ;; 176) SLUG=third ;; esac
  printf '# TASK %s: Fixture\n' "${ID}" >"${PRIMARY}/docs/tasks/${ID}-${SLUG}.md"
done
printf 'first\n' >"${PRIMARY}/first.txt"
printf 'second\n' >"${PRIMARY}/second.txt"
cp "${TOOL}" "${PRIMARY}/scripts/utils/manage-write-lease.sh"
cp "${SOURCE_ROOT}/scripts/utils/run-staged-gate.sh" "${PRIMARY}/scripts/utils/run-staged-gate.sh"
cat >"${PRIMARY}/scripts/tests/test.sh" <<'FIXTURE'
#!/usr/bin/env bash
# MAILCLI_GATE_HARNESS=staged-v1
set -euo pipefail
case "${1:-}" in
  --list-shell-tests) printf 'scripts/tests/test-worker.sh\n'; exit 0 ;;
  --list-live-shell-tests) exit 0 ;;
  --core-source) printf 'worker_core() { test -f "${MAILCLI_ROOT}/first.txt"; }\n'; exit 0 ;;
esac
[[ "${1:-}" == --checks ]]
"${MAILCLI_ROOT}/scripts/tests/test-worker.sh"
printf '%s\tscripts/tests/test-worker.sh\n' "$(shasum -a 256 "${MAILCLI_ROOT}/scripts/tests/test-worker.sh" | awk '{print $1}')" >>"${MAILCLI_GATE_RECEIPTS}"
FIXTURE
printf '#!/usr/bin/env bash\nset -euo pipefail\ntest -s "${MAILCLI_ROOT}/first.txt"\n' >"${PRIMARY}/scripts/tests/test-worker.sh"
chmod 755 "${PRIMARY}/scripts/"{tests,utils}/*.sh
stage_path() {
  local ROOT="$1" PATH_NAME="$2" MODE=100644 BLOB
  [[ ! -x "${ROOT}/${PATH_NAME}" ]] || MODE=100755
  BLOB="$(git -C "${ROOT}" hash-object -w "${ROOT}/${PATH_NAME}")"
  git -C "${ROOT}" update-index --add --cacheinfo "${MODE},${BLOB},${PATH_NAME}"
}
for PATH_NAME in first.txt second.txt scripts/utils/manage-write-lease.sh scripts/utils/run-staged-gate.sh scripts/tests/test.sh scripts/tests/test-worker.sh; do
  stage_path "${PRIMARY}" "${PATH_NAME}"
done
BASE="$(printf 'fixture baseline\n' | git -C "${PRIMARY}" commit-tree "$(git -C "${PRIMARY}" write-tree)")"
git -C "${PRIMARY}" checkout -q --detach "${BASE}"
lease() { MAILCLI_WRITE_ROOT="$1" "${TOOL}" "${@:2}"; }
reject() {
  local EXPECTED="$1" OUTPUT
  shift
  if OUTPUT="$("$@" 2>&1)"; then printf 'Unexpected acceptance: %s\n' "$*" >&2; exit 1; fi
  [[ "${OUTPUT}" == *"${EXPECTED}"* ]] || { printf 'Wrong rejection: %s\n' "${OUTPUT}" >&2; exit 1; }
}
reject 'primary HEAD is stale' lease "${PRIMARY}" start-worktree 174 first 0000000000000000000000000000000000000000 "${TEST_ROOT}/stale" first.txt
[[ ! -e "${TEST_ROOT}/stale" ]]
mv "${PRIMARY}/AGENTS.local.md" "${TEST_ROOT}/rules"
reject 'Missing regular worker contract' lease "${PRIMARY}" start-worktree 174 first "${BASE}" "${TEST_ROOT}/missing" first.txt
[[ ! -e "${TEST_ROOT}/missing" ]]
mv "${TEST_ROOT}/rules" "${PRIMARY}/AGENTS.local.md"
FIRST="${TEST_ROOT}/first-worker"; SECOND="${TEST_ROOT}/second-worker"
FIRST_OUTPUT="$(lease "${PRIMARY}" start-worktree 174 first "${BASE}" "${FIRST}" first.txt ignored/parent)"
FIRST_TOKEN="$(sed -n 's/^write_lease_token=//p' <<<"${FIRST_OUTPUT}")"
SECOND_OUTPUT="$(lease "${PRIMARY}" start-worktree 175 second "${BASE}" "${SECOND}" second.txt)"
SECOND_TOKEN="$(sed -n 's/^write_lease_token=//p' <<<"${SECOND_OUTPUT}")"
[[ ! -e "${FIRST}/docs/tasks.md" ]]
cmp "${PRIMARY}/AGENTS.local.md" "${FIRST}/AGENTS.local.md"
reject 'TASK 174 is already reserved' lease "${PRIMARY}" acquire 174 competitor third.txt
reject 'Path overlaps another reservation' lease "${PRIMARY}" acquire 176 competitor FIRST.txt
reject 'Path overlaps another reservation' lease "${PRIMARY}" acquire 176 competitor ignored/parent/child
mkdir "${PRIMARY}/.git/mailcli-write-reservations.lock"
reject 'registry is busy' lease "${PRIMARY}" acquire 176 competitor third.txt
rmdir "${PRIMARY}/.git/mailcli-write-reservations.lock"
printf 'changed contract\n' >>"${PRIMARY}/docs/tasks/174-first.md"
reject 'contract or owner instructions changed' lease "${FIRST}" abort "${FIRST_TOKEN}"
cp "${FIRST}/docs/tasks/174-first.md" "${TEST_ROOT}/contract"
mv "${PRIMARY}/docs/tasks/174-first.md" "${TEST_ROOT}/changed-contract"
mv "${TEST_ROOT}/contract" "${PRIMARY}/docs/tasks/174-first.md"
lease "${SECOND}" abort "${SECOND_TOKEN}" >/dev/null
printf 'worker change\n' >"${FIRST}/first.txt"
stage_path "${FIRST}" first.txt
lease "${FIRST}" review "${FIRST_TOKEN}" >/dev/null
lease "${FIRST}" gate "${FIRST_TOKEN}" --checks scripts/tests/test-worker.sh >"${TEST_ROOT}/worker-gate"
grep -qx 'full_gate=deferred' "${TEST_ROOT}/worker-gate"
WORKER_COMMIT="$(printf 'TASK 174: worker change\n' | git -C "${FIRST}" commit-tree "$(git -C "${FIRST}" write-tree)" -p "${BASE}")"
REBASE_PARENT="$(printf 'intervening base\n' | git -C "${FIRST}" commit-tree "$(git -C "${FIRST}" rev-parse HEAD^{tree})" -p "${BASE}")"
REBASED_COMMIT="$(printf 'TASK 174: rebased worker\n' | git -C "${FIRST}" commit-tree "$(git -C "${FIRST}" write-tree)" -p "${REBASE_PARENT}")"
git -C "${FIRST}" checkout -q --detach "${REBASED_COMMIT}"
reject 'parent is the acquired HEAD' lease "${FIRST}" release "${FIRST_TOKEN}"
git -C "${FIRST}" checkout -q --detach "${WORKER_COMMIT}"
lease "${FIRST}" release "${FIRST_TOKEN}" >"${TEST_ROOT}/handoff"
grep -qx "integration_commit=${WORKER_COMMIT}" "${TEST_ROOT}/handoff"
reject 'Path overlaps another reservation' lease "${PRIMARY}" acquire 176 competitor first.txt
MAILCLI_WRITE_ROOT="${PRIMARY}" "${SOURCE_ROOT}/scripts/utils/manage-full-proof.sh" check "${PRIMARY}" >"${TEST_ROOT}/full-output" 2>&1 && exit 1
grep -q 'no shared worker reservations' "${TEST_ROOT}/full-output"
lease "${PRIMARY}" integrate "${FIRST_TOKEN}" coordinator >"${TEST_ROOT}/integration"
git -C "${PRIMARY}" cherry-pick --no-commit "${WORKER_COMMIT}"
reject 'No successful gate evidence' lease "${PRIMARY}" release "${FIRST_TOKEN}"
lease "${PRIMARY}" review "${FIRST_TOKEN}" >/dev/null
lease "${PRIMARY}" gate "${FIRST_TOKEN}" --checks scripts/tests/test-worker.sh >"${TEST_ROOT}/integration-gate"
INTEGRATED="$(printf 'TASK 174: integrated worker change\n' | git -C "${PRIMARY}" commit-tree "$(git -C "${PRIMARY}" write-tree)" -p "${BASE}")"
git -C "${PRIMARY}" checkout -q --detach "${INTEGRATED}"
lease "${PRIMARY}" release "${FIRST_TOKEN}" >/dev/null
[[ -z "$(find "${PRIMARY}/.git/mailcli-write-reservations" -type f -print)" ]]
set +e
lease "${PRIMARY}" start-worktree 175 race-one "${INTEGRATED}" "${TEST_ROOT}/race-one" second.txt >"${TEST_ROOT}/race-one-output" 2>&1 &
RACE_ONE_PID=$!
lease "${PRIMARY}" start-worktree 176 race-two "${INTEGRATED}" "${TEST_ROOT}/race-two" second.txt >"${TEST_ROOT}/race-two-output" 2>&1 &
RACE_TWO_PID=$!
wait "${RACE_ONE_PID}"; RACE_ONE_STATUS=$?
wait "${RACE_TWO_PID}"; RACE_TWO_STATUS=$?
set -e
[[ ( "${RACE_ONE_STATUS}" == 0 && "${RACE_TWO_STATUS}" != 0 ) || ( "${RACE_ONE_STATUS}" != 0 && "${RACE_TWO_STATUS}" == 0 ) ]]
if [[ "${RACE_ONE_STATUS}" == 0 ]]; then WINNER=race-one; LOSER=race-two; else WINNER=race-two; LOSER=race-one; fi
grep -Eq 'Path overlaps another reservation|registry is busy' "${TEST_ROOT}/${LOSER}-output"
RACE_TOKEN="$(sed -n 's/^write_lease_token=//p' "${TEST_ROOT}/${WINNER}-output")"
lease "${TEST_ROOT}/${WINNER}" abort "${RACE_TOKEN}" >/dev/null
[[ -z "$(find "${PRIMARY}/.git/mailcli-write-reservations" -type f -print)" ]]
printf 'Worktree coordination passed: shared ownership, disjoint workers, contract binding, stale start and fresh serial integration\n'

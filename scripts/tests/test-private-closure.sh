#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-private-closure.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
TEST_REPOSITORY="${TEST_ROOT}/repo"
BACKUP_ROOT="${TEST_ROOT}/backups"
LEASE_TOOL="${TEST_REPOSITORY}/scripts/utils/manage-write-lease.sh"
EXPORT_TOOL="${TEST_REPOSITORY}/scripts/utils/export-task-history.sh"
mkdir -p "${TEST_REPOSITORY}/scripts/utils" \
  "${TEST_REPOSITORY}/scripts/tests" \
  "${TEST_REPOSITORY}/docs/tasks/done" \
  "${TEST_REPOSITORY}/ignored/tree" "${BACKUP_ROOT}"
chmod 700 "${BACKUP_ROOT}"
cp "${MAILCLI_ROOT}/scripts/utils/manage-write-lease.sh" "${LEASE_TOOL}"
cp "${MAILCLI_ROOT}/scripts/utils/export-task-history.sh" "${EXPORT_TOOL}"
chmod 755 "${LEASE_TOOL}" "${EXPORT_TOOL}"

git -C "${TEST_REPOSITORY}" init -q -b main
git -C "${TEST_REPOSITORY}" config user.name "MailCLI Test"
git -C "${TEST_REPOSITORY}" config user.email "mailcli-test@example.invalid"
printf 'tracked baseline\n' >"${TEST_REPOSITORY}/tracked.txt"
printf '%s\n' '#!/usr/bin/env bash' \
  'MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"' \
  'exit 0' >"${TEST_REPOSITORY}/scripts/tests/test.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"
for RELATIVE_PATH in tracked.txt scripts/utils/manage-write-lease.sh \
  scripts/utils/export-task-history.sh scripts/tests/test.sh; do
  BLOB="$(git -C "${TEST_REPOSITORY}" hash-object -w \
    "${TEST_REPOSITORY}/${RELATIVE_PATH}")"
  MODE=100644
  [[ "${RELATIVE_PATH}" == scripts/* ]] && MODE=100755
  git -C "${TEST_REPOSITORY}" update-index --add --cacheinfo \
    "${MODE},${BLOB},${RELATIVE_PATH}"
done
INITIAL_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
INITIAL_COMMIT="$(printf 'initial\n' | git -C "${TEST_REPOSITORY}" \
  commit-tree "${INITIAL_TREE}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${INITIAL_COMMIT}"
printf '/docs/tasks.md\n/docs/tasks/\n/ignored/\n' \
  >"${TEST_REPOSITORY}/.git/info/exclude"

printf '%s\n' \
  '# MailCLI Tasks' '' '## Active' \
  '- [~] 175 Private closure -> tasks/175-private.md' \
  '' '## Queue' \
  '- [ ] 176 Next -> tasks/176-next.md' \
  '' '## Blocked' '' '## Done' \
  '- [x] 109 Old -> tasks/109-old.md' >"${TEST_REPOSITORY}/docs/tasks.md"
printf '%s\n' '# TASK 109: Old' >"${TEST_REPOSITORY}/docs/tasks/109-old.md"
printf '%s\n' \
  '# TASK 175: Private closure' '' \
  '## Why' '' 'Private fixture.' '' \
  '## Acceptance' '' '- Archive and verify.' '' \
  '## Sub-Tasks' '' '- [x] Prepare the exact receipt.' '' \
  '## Notes' '' 'Fixture evidence.' '' \
  '## Deviations' '' '- none.' >"${TEST_REPOSITORY}/docs/tasks/175-private.md"
printf '%s\n' '# TASK 176: Next' >"${TEST_REPOSITORY}/docs/tasks/176-next.md"
printf 'obsolete\n' >"${TEST_REPOSITORY}/ignored/old.txt"
printf 'child\n' >"${TEST_REPOSITORY}/ignored/tree/child.txt"
printf 'preserve\n' >"${TEST_REPOSITORY}/ignored/keep.txt"

EXPECTED_PATHS=(
  docs/tasks.md
  docs/tasks/109-old.md
  docs/tasks/done/109-old.md
  docs/tasks/175-private.md
  docs/tasks/done/175-private.md
  ignored/old.txt
  ignored/tree
  ignored/tree/child.txt
)
ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" acquire 175 fixture-owner "${EXPECTED_PATHS[@]}")"
TOKEN="$(printf '%s\n' "${ACQUIRE_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
[[ -n "${TOKEN}" ]]
STALE_OUTPUT="$("${EXPORT_TOOL}" create "${BACKUP_ROOT}")"
STALE_SNAPSHOT="$(printf '%s\n' "${STALE_OUTPUT}" |
  sed -n 's/^snapshot_created=//p')"

mv "${TEST_REPOSITORY}/docs/tasks/109-old.md" \
  "${TEST_REPOSITORY}/docs/tasks/done/109-old.md"
mv "${TEST_REPOSITORY}/docs/tasks/175-private.md" \
  "${TEST_REPOSITORY}/docs/tasks/done/175-private.md"
rm "${TEST_REPOSITORY}/ignored/old.txt" \
  "${TEST_REPOSITORY}/ignored/tree/child.txt"
rmdir "${TEST_REPOSITORY}/ignored/tree"
printf '%s\n' \
  '# MailCLI Tasks' '' '## Active' \
  '- [~] 176 Next -> tasks/176-next.md' \
  '' '## Queue' '' '## Blocked' '' '## Done' \
  '- [x] 175 Private closure -> tasks/done/175-private.md' \
  '- [x] 109 Old -> tasks/done/109-old.md' >"${TEST_REPOSITORY}/docs/tasks.md"
FRESH_OUTPUT="$("${EXPORT_TOOL}" create "${BACKUP_ROOT}")"
FRESH_SNAPSHOT="$(printf '%s\n' "${FRESH_OUTPUT}" |
  sed -n 's/^snapshot_created=//p')"

expect_failure() {
  local NAME="$1"
  local NEEDLE="$2"
  local SNAPSHOT="$3"
  shift 3
  local OUTPUT
  if OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
    "${LEASE_TOOL}" private-proof "${TOKEN}" "${SNAPSHOT}" "$@" 2>&1)"; then
    printf '%s unexpectedly proved private closure\n' "${NAME}" >&2
    exit 1
  fi
  [[ "${OUTPUT}" == *"${NEEDLE}"* ]] || {
    printf '%s failed for the wrong reason: %s\n' "${NAME}" "${OUTPUT}" >&2
    exit 1
  }
}

expect_failure stale 'snapshot is stale' "${STALE_SNAPSHOT}" "${EXPECTED_PATHS[@]}"
expect_failure missing 'Snapshot directory is missing' \
  "${BACKUP_ROOT}/missing" "${EXPECTED_PATHS[@]}"
if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" private-proof wrong-token "${FRESH_SNAPSHOT}" \
  "${EXPECTED_PATHS[@]}" >/dev/null 2>&1; then
  printf 'Wrong owner token proved private closure\n' >&2
  exit 1
fi
expect_failure wrong_scope 'Changed paths differ' "${FRESH_SNAPSHOT}" \
  "${EXPECTED_PATHS[@]:0:7}"
printf 'edited after move\n' >"${TEST_REPOSITORY}/docs/tasks/done/109-old.md"
expect_failure changed_move 'Archived task move changed content' \
  "${FRESH_SNAPSHOT}" "${EXPECTED_PATHS[@]}"
printf '%s\n' '# TASK 109: Old' \
  >"${TEST_REPOSITORY}/docs/tasks/done/109-old.md"
printf 'changed\n' >"${TEST_REPOSITORY}/ignored/keep.txt"
expect_failure unrelated 'Ignored asset changed outside' \
  "${FRESH_SNAPSHOT}" "${EXPECTED_PATHS[@]}"
printf 'preserve\n' >"${TEST_REPOSITORY}/ignored/keep.txt"
printf 'tracked change\n' >"${TEST_REPOSITORY}/tracked.txt"
expect_failure tracked 'Tracked worktree is not clean' \
  "${FRESH_SNAPSHOT}" "${EXPECTED_PATHS[@]}"
printf 'tracked baseline\n' >"${TEST_REPOSITORY}/tracked.txt"

BAD_SNAPSHOT="${TEST_ROOT}/bad-snapshot"
cp -pR "${FRESH_SNAPSHOT}" "${BAD_SNAPSHOT}"
printf 'corruption\n' >>"${BAD_SNAPSHOT}/MANIFEST.sha256"
expect_failure corrupt_manifest 'Task snapshot is invalid' \
  "${BAD_SNAPSHOT}" "${EXPECTED_PATHS[@]}"

PROOF_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" private-proof "${TOKEN}" "${FRESH_SNAPSHOT}" \
  "${EXPECTED_PATHS[@]}")"
[[ "${PROOF_OUTPUT}" == *'private_closure=verified'* ]]
RECEIPT_PATH="$(printf '%s\n' "${PROOF_OUTPUT}" |
  sed -n 's/^private_receipt_path=//p')"
[[ -f "${RECEIPT_PATH}" && "$(stat -f '%Lp' "${RECEIPT_PATH}")" == 600 ]]
grep -Fxq 'task=175' "${RECEIPT_PATH}"
grep -Fq $'allowed_path_change\tignored/tree\tdirectory\tabsent' \
  "${RECEIPT_PATH}"
expect_failure no_overwrite 'Private closure receipt already exists' \
  "${FRESH_SNAPSHOT}" "${EXPECTED_PATHS[@]}"

ABORT_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" abort "${TOKEN}")"
diff -u \
  <(printf '%s\n' "${PROOF_OUTPUT}" | grep '^allowed_path_change') \
  <(printf '%s\n' "${ABORT_OUTPUT}" | grep '^allowed_path_change')
[[ "$(git -C "${TEST_REPOSITORY}" rev-parse HEAD)" == "${INITIAL_COMMIT}" ]]
[[ -z "$(git -C "${TEST_REPOSITORY}" status --porcelain=v1 --untracked-files=all)" ]]
"${EXPORT_TOOL}" verify "${FRESH_SNAPSHOT}" >/dev/null

# These assertions belong to the private-proof feature, rather than the retained
# product/asset coordination suite. TASK 509 may retire only this whole feature.
expect_private_scope_failure() {
  local COMMAND="$1"
  local OWNER_TOKEN="$2"
  local OUTPUT
  if OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
    "${LEASE_TOOL}" "${COMMAND}" "${OWNER_TOKEN}" 2>&1)"; then
    printf '%s accepted an out-of-scope ignored task change\n' "${COMMAND}" >&2
    exit 1
  fi
  [[ "${OUTPUT}" == *'Private task path changed outside the lease allowlist'* ]]
}
ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" acquire 176 scope-owner tracked.txt)"
TOKEN="$(printf '%s\n' "${ACQUIRE_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
printf 'tracked scope change\n' >"${TEST_REPOSITORY}/tracked.txt"
BLOB="$(git -C "${TEST_REPOSITORY}" hash-object -w "${TEST_REPOSITORY}/tracked.txt")"
git -C "${TEST_REPOSITORY}" update-index --cacheinfo "100644,${BLOB},tracked.txt"
cp "${TEST_REPOSITORY}/docs/tasks.md" "${TEST_ROOT}/scope-board"
printf 'board changed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
expect_private_scope_failure review "${TOKEN}"
cp "${TEST_ROOT}/scope-board" "${TEST_REPOSITORY}/docs/tasks.md"
printf 'added\n' >"${TEST_REPOSITORY}/docs/tasks/177-added.md"
expect_private_scope_failure review "${TOKEN}"
rm "${TEST_REPOSITORY}/docs/tasks/177-added.md"
mv "${TEST_REPOSITORY}/docs/tasks/176-next.md" "${TEST_ROOT}/scope-detail"
expect_private_scope_failure review "${TOKEN}"
mv "${TEST_ROOT}/scope-detail" "${TEST_REPOSITORY}/docs/tasks/176-moved.md"
expect_private_scope_failure review "${TOKEN}"
mv "${TEST_REPOSITORY}/docs/tasks/176-moved.md" "${TEST_REPOSITORY}/docs/tasks/176-next.md"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" "${LEASE_TOOL}" review "${TOKEN}" >/dev/null
printf 'board changed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
expect_private_scope_failure gate "${TOKEN}"
cp "${TEST_ROOT}/scope-board" "${TEST_REPOSITORY}/docs/tasks.md"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" "${LEASE_TOOL}" gate "${TOKEN}" >/dev/null
SCOPE_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
SCOPE_COMMIT="$(printf 'TASK 176: private scope fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${SCOPE_TREE}" -p "${INITIAL_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${SCOPE_COMMIT}"
printf 'board changed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
expect_private_scope_failure release "${TOKEN}"
cp "${TEST_ROOT}/scope-board" "${TEST_REPOSITORY}/docs/tasks.md"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" "${LEASE_TOOL}" release "${TOKEN}" >/dev/null
ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" acquire 176 private-owner docs/tasks.md)"
TOKEN="$(printf '%s\n' "${ACQUIRE_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
cp "${TEST_REPOSITORY}/docs/tasks/176-next.md" "${TEST_ROOT}/scope-detail"
printf 'detail changed\n' >"${TEST_REPOSITORY}/docs/tasks/176-next.md"
expect_private_scope_failure abort "${TOKEN}"
cp "${TEST_ROOT}/scope-detail" "${TEST_REPOSITORY}/docs/tasks/176-next.md"
printf 'board allowed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" "${LEASE_TOOL}" abort "${TOKEN}" >/dev/null

printf 'Private closure passed: exact move and cleanup scope, fresh owner-only snapshot, durable receipt, and independent abort comparison\n'

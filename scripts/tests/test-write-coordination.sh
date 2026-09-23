#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
LEASE_TOOL="${MAILCLI_ROOT}/scripts/utils/manage-write-lease.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-write-coordination.XXXXXX")"
TEST_REPOSITORY="${TEST_ROOT}/repo"
TEST_REPOSITORY_ALIAS=""
OTHER_REPOSITORY="${TEST_ROOT}/other-worktree"

cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-write-coordination."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

mkdir -p "${TEST_REPOSITORY}/scripts/tests" "${TEST_REPOSITORY}/scripts/utils"
git -C "${TEST_REPOSITORY}" init -q -b main
git -C "${TEST_REPOSITORY}" config user.name "MailCLI Test"
git -C "${TEST_REPOSITORY}" config user.email "mailcli-test@example.invalid"
printf 'original\n' >"${TEST_REPOSITORY}/tracked.txt"
printf 'other\n' >"${TEST_REPOSITORY}/other.txt"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"' \
  'printf "baseline-harness\\n"' \
  "exit \"\${MAILCLI_TEST_GATE_STATUS:-0}\"" >"${TEST_REPOSITORY}/scripts/tests/test.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'printf "standalone-reporter\\n"' >"${TEST_REPOSITORY}/scripts/tests/report-task-ci.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/report-task-ci.sh"
cp "${LEASE_TOOL}" "${TEST_REPOSITORY}/scripts/utils/manage-write-lease.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/utils/manage-write-lease.sh"

# Exercise the lease through a differently spelled path for the same worktree.
# On case-insensitive filesystems the uppercase spelling resolves to the same
# directory; elsewhere a symlink provides the equivalent alias coverage.
if [[ -d "${TEST_ROOT}/REPO" ]]; then
  TEST_REPOSITORY_ALIAS="${TEST_ROOT}/REPO"
else
  TEST_REPOSITORY_ALIAS="${TEST_ROOT}/repo-alias"
  ln -s "${TEST_REPOSITORY}" "${TEST_REPOSITORY_ALIAS}"
fi

mkdir -p "${OTHER_REPOSITORY}"
git -C "${OTHER_REPOSITORY}" init -q -b main

stage_fixture_path() {
  local RELATIVE_PATH="$1"
  local MODE="$2"
  local BLOB
  BLOB="$(git -C "${TEST_REPOSITORY}" hash-object -w "${TEST_REPOSITORY}/${RELATIVE_PATH}")"
  git -C "${TEST_REPOSITORY}" update-index --add --cacheinfo \
    "${MODE},${BLOB},${RELATIVE_PATH}"
}

stage_fixture_path tracked.txt 100644
stage_fixture_path other.txt 100644
stage_fixture_path scripts/tests/test.sh 100755
stage_fixture_path scripts/tests/report-task-ci.sh 100755
stage_fixture_path scripts/utils/manage-write-lease.sh 100755
INITIAL_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
INITIAL_COMMIT="$(printf 'initial\n' | git -C "${TEST_REPOSITORY}" commit-tree "${INITIAL_TREE}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${INITIAL_COMMIT}"
mkdir -p "${TEST_REPOSITORY}/docs/tasks"
printf 'board baseline\n' >"${TEST_REPOSITORY}/docs/tasks.md"
printf 'detail baseline\n' >"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
printf '/docs/tasks.md\n/docs/tasks/\n/ignored/\n' >"${TEST_REPOSITORY}/.git/info/exclude"
mkdir -p "${TEST_REPOSITORY}/ignored/nested" "${TEST_REPOSITORY}/ignored/empty"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
printf 'nested\n' >"${TEST_REPOSITORY}/ignored/nested/keep.txt"

expect_private_scope_failure() {
  local COMMAND="$1"
  local OWNER_TOKEN="$2"
  if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${LEASE_TOOL}" "${COMMAND}" "${OWNER_TOKEN}" >/dev/null 2>&1; then
    printf '%s accepted an out-of-scope ignored task change\n' "${COMMAND}" >&2
    exit 1
  fi
}

expect_ignored_scope_failure() {
  local COMMAND="$1"
  local OWNER_TOKEN="$2"
  local OUTPUT
  if OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${LEASE_TOOL}" "${COMMAND}" "${OWNER_TOKEN}" 2>&1)"; then
    printf '%s accepted an out-of-scope ignored asset change\n' "${COMMAND}" >&2
    exit 1
  fi
  if [[ "${OUTPUT}" != *'Ignored asset changed outside the lease allowlist'* ]]; then
    printf '%s failed for the wrong reason: %s\n' "${COMMAND}" "${OUTPUT}" >&2
    exit 1
  fi
}

ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 174 test-writer tracked.txt)"
TOKEN="$(printf '%s\n' "${ACQUIRE_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
[[ -n "${TOKEN}" ]]

# The canonical worktree spelling sees the lease that was acquired through the
# differently spelled alias path.
STATUS_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" status)"
printf '%s\n' "${STATUS_OUTPUT}" | grep -Fq 'write_lease=active' || {
  printf 'Canonical worktree path did not see the alias-acquired lease\n' >&2
  exit 1
}
printf '%s\n' "${STATUS_OUTPUT}" | grep -Fq 'task=174' || {
  printf 'Canonical worktree path reported the wrong lease task\n' >&2
  exit 1
}

# A genuinely different worktree, a subdirectory inside the worktree, and a
# symlink escaping the worktree are all rejected at their own boundaries.
if MAILCLI_WRITE_ROOT="${OTHER_REPOSITORY}" \
  "${LEASE_TOOL}" review "${TOKEN}" >/dev/null 2>&1; then
  printf 'Review accepted a token in a different worktree\n' >&2
  exit 1
fi
if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}/scripts" \
  "${LEASE_TOOL}" status >/dev/null 2>&1; then
  printf 'Status accepted a subdirectory as the worktree root\n' >&2
  exit 1
fi
ln -s "${TEST_ROOT}" "${TEST_ROOT}/escape-link"
if MAILCLI_WRITE_ROOT="${TEST_ROOT}/escape-link" \
  "${LEASE_TOOL}" status >/dev/null 2>&1; then
  printf 'Status accepted a symlink escaping the worktree\n' >&2
  exit 1
fi

if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175 competing-writer other.txt >/dev/null 2>&1; then
  printf 'A second writer acquired the same worktree lease\n' >&2
  exit 1
fi

printf 'unauthorized\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${TOKEN}" >/dev/null 2>&1; then
  printf 'Review accepted a staged path outside the lease allowlist\n' >&2
  exit 1
fi

printf 'other\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
printf 'changed\n' >"${TEST_REPOSITORY}/tracked.txt"
stage_fixture_path tracked.txt 100644
printf 'board changed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
expect_private_scope_failure review "${TOKEN}"
printf 'board baseline\n' >"${TEST_REPOSITORY}/docs/tasks.md"
printf 'added\n' >"${TEST_REPOSITORY}/docs/tasks/175-added.md"
expect_private_scope_failure review "${TOKEN}"
rm "${TEST_REPOSITORY}/docs/tasks/175-added.md"
rm "${TEST_REPOSITORY}/docs/tasks/174-detail.md"
expect_private_scope_failure review "${TOKEN}"
printf 'detail baseline\n' >"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
mv "${TEST_REPOSITORY}/docs/tasks/174-detail.md" \
  "${TEST_REPOSITORY}/docs/tasks/174-moved.md"
expect_private_scope_failure review "${TOKEN}"
mv "${TEST_REPOSITORY}/docs/tasks/174-moved.md" \
  "${TEST_REPOSITORY}/docs/tasks/174-detail.md"
printf 'changed\n' >"${TEST_REPOSITORY}/ignored/one.txt"
expect_ignored_scope_failure review "${TOKEN}"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
printf 'added\n' >"${TEST_REPOSITORY}/ignored/added.txt"
expect_ignored_scope_failure review "${TOKEN}"
rm "${TEST_REPOSITORY}/ignored/added.txt"
mv "${TEST_REPOSITORY}/ignored/nested/keep.txt" \
  "${TEST_REPOSITORY}/ignored/nested/moved.txt"
expect_ignored_scope_failure review "${TOKEN}"
mv "${TEST_REPOSITORY}/ignored/nested/moved.txt" \
  "${TEST_REPOSITORY}/ignored/nested/keep.txt"
rm "${TEST_REPOSITORY}/ignored/nested/keep.txt"
expect_ignored_scope_failure review "${TOKEN}"
printf 'nested\n' >"${TEST_REPOSITORY}/ignored/nested/keep.txt"
mkdir "${TEST_REPOSITORY}/ignored/empty/new"
expect_ignored_scope_failure review "${TOKEN}"
rmdir "${TEST_REPOSITORY}/ignored/empty/new"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${TOKEN}" >/dev/null

printf 'changed\n' >"${TEST_REPOSITORY}/ignored/one.txt"
expect_ignored_scope_failure gate "${TOKEN}"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
printf 'board changed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
expect_private_scope_failure gate "${TOKEN}"
printf 'board baseline\n' >"${TEST_REPOSITORY}/docs/tasks.md"

GATE_STATUS=0
MAILCLI_TEST_GATE_STATUS=23 MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${TOKEN}" >/dev/null 2>&1 || GATE_STATUS=$?
if [[ "${GATE_STATUS}" -ne 23 ]]; then
  printf 'Failing full gate status was not preserved: %s\n' "${GATE_STATUS}" >&2
  exit 1
fi
if [[ -e "${TEST_REPOSITORY}/.git/mailcli-write-lease/gate_patch_sha256" ]]; then
  printf 'Failing full gate recorded commit evidence\n' >&2
  exit 1
fi

MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${TOKEN}" >/dev/null
TASK_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
TASK_COMMIT="$(printf 'TASK 174: coordination fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${TASK_TREE}" -p "${INITIAL_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${TASK_COMMIT}"
printf 'changed\n' >"${TEST_REPOSITORY}/ignored/one.txt"
expect_ignored_scope_failure release "${TOKEN}"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
printf 'board changed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
expect_private_scope_failure release "${TOKEN}"
printf 'board baseline\n' >"${TEST_REPOSITORY}/docs/tasks.md"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${TOKEN}" >/dev/null
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

ABORT_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175 abort-owner other.txt)"
ABORT_TOKEN="$(printf '%s\n' "${ABORT_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
IGNORED_BASELINE="${TEST_REPOSITORY}/.git/mailcli-write-lease/ignored_asset_fingerprints"
cp "${IGNORED_BASELINE}" "${TEST_ROOT}/ignored-asset-baseline"
rm "${IGNORED_BASELINE}"
MISSING_BASELINE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" abort "${ABORT_TOKEN}" 2>&1)" && {
  printf 'Abort accepted a missing ignored-asset baseline\n' >&2
  exit 1
}
[[ "${MISSING_BASELINE_OUTPUT}" == *'Ignored asset baseline is missing'* ]]
mv "${TEST_ROOT}/ignored-asset-baseline" "${IGNORED_BASELINE}"
if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" abort wrong-token >/dev/null 2>&1; then
  printf 'Wrong owner token aborted the active write lease\n' >&2
  exit 1
fi
printf 'changed\n' >"${TEST_REPOSITORY}/ignored/one.txt"
expect_ignored_scope_failure abort "${ABORT_TOKEN}"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" abort "${ABORT_TOKEN}" >/dev/null

CLEANUP_ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175 cleanup-owner \
  ignored/one.txt ignored/nested ignored/nested/keep.txt)"
CLEANUP_TOKEN="$(printf '%s\n' "${CLEANUP_ACQUIRE_OUTPUT}" |
  sed -n 's/^write_lease_token=//p')"
printf 'updated\n' >"${TEST_REPOSITORY}/ignored/one.txt"
printf 'surprise\n' >"${TEST_REPOSITORY}/ignored/nested/surprise.txt"
expect_ignored_scope_failure abort "${CLEANUP_TOKEN}"
rm "${TEST_REPOSITORY}/ignored/nested/surprise.txt"
rm "${TEST_REPOSITORY}/ignored/nested/keep.txt"
rmdir "${TEST_REPOSITORY}/ignored/nested"
CLEANUP_ABORT_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" abort "${CLEANUP_TOKEN}")"
printf '%s\n' "${CLEANUP_ABORT_OUTPUT}" |
  grep -Fq $'allowed_path_change\tignored/nested\tdirectory\tabsent'
printf '%s\n' "${CLEANUP_ABORT_OUTPUT}" |
  grep -Eq $'allowed_path_change\tignored/nested/keep.txt\tfile:[0-9a-f]{64}\tabsent'

PRIVATE_ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175 private-owner docs/tasks.md)"
PRIVATE_TOKEN="$(printf '%s\n' "${PRIVATE_ACQUIRE_OUTPUT}" |
  sed -n 's/^write_lease_token=//p')"
printf 'detail changed\n' >"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
expect_private_scope_failure abort "${PRIVATE_TOKEN}"
printf 'detail baseline\n' >"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
printf 'board allowed\n' >"${TEST_REPOSITORY}/docs/tasks.md"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" abort "${PRIVATE_TOKEN}" >/dev/null

HARNESS_ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 176 harness-writer scripts/tests/test.sh)"
HARNESS_TOKEN="$(printf '%s\n' "${HARNESS_ACQUIRE_OUTPUT}" |
  sed -n 's/^write_lease_token=//p')"
[[ -n "${HARNESS_TOKEN}" ]]
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"' \
  'printf "patched-harness\\n"' \
  "printf 'x' >\"${TEST_REPOSITORY}/patched-harness-ran\"" \
  "exit \"\${MAILCLI_TEST_GATE_STATUS:-0}\"" >"${TEST_REPOSITORY}/scripts/tests/test.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"
stage_fixture_path scripts/tests/test.sh 100755
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${HARNESS_TOKEN}" >/dev/null
GATE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${HARNESS_TOKEN}" 2>&1)"
printf '%s\n' "${GATE_OUTPUT}" | grep -Fq 'baseline-harness' || {
  printf 'Gate did not run the baseline harness\n' >&2
  exit 1
}
printf '%s\n' "${GATE_OUTPUT}" | grep -Fq 'gate_harness=baseline' || {
  printf 'Gate did not report baseline harness selection\n' >&2
  exit 1
}
if printf '%s\n' "${GATE_OUTPUT}" | grep -Fq 'patched-harness'; then
  printf 'Gate ran the staged harness patch instead of the baseline harness\n' >&2
  exit 1
fi
[[ ! -e "${TEST_REPOSITORY}/patched-harness-ran" ]] || {
  printf 'Staged harness patch executed inside the gate\n' >&2
  exit 1
}
TASK_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
TASK_COMMIT="$(printf 'TASK 176: baseline harness fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${TASK_TREE}" -p "${TASK_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${TASK_COMMIT}"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${HARNESS_TOKEN}" >/dev/null
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

printf 'Write coordination passed: one writer, tracked and ignored asset scope, bounded directory cleanup, failure-preserving gate, baseline-bound harness, and tested commit identity\n'

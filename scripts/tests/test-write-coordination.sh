#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
LEASE_TOOL="${MAILCLI_ROOT}/scripts/utils/manage-write-lease.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-write-coordination.XXXXXX")"
TEST_REPOSITORY="${TEST_ROOT}/repo#[literal]"
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
write_fixture_harness() {
  cat >"${TEST_REPOSITORY}/scripts/tests/test.sh" <<'FIXTURE'
#!/usr/bin/env bash
# MAILCLI_GATE_HARNESS=staged-v1
set -euo pipefail
ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
core_check() {
  test -f "${ROOT}/tracked.txt"
}
case "${1:-}" in
  --list-shell-tests) printf 'scripts/tests/test-lease-fixture.sh\n'; exit 0 ;;
  --list-live-shell-tests) exit 0 ;;
  --core-source) declare -f core_check; exit 0 ;;
  --core-only) core_check; exit 0 ;;
esac
printf 'baseline-harness\n'
if [[ "${1:-}" != --checks ]]; then
  core_check
  printf '%s\tcore-checks\n' "$(declare -f core_check | shasum -a 256 | awk '{print $1}')" >>"${MAILCLI_GATE_RECEIPTS}"
fi
"${ROOT}/scripts/tests/test-lease-fixture.sh" || exit "$?"
printf '%s\tscripts/tests/test-lease-fixture.sh\n' "$(shasum -a 256 "${ROOT}/scripts/tests/test-lease-fixture.sh" | awk '{print $1}')" >>"${MAILCLI_GATE_RECEIPTS}"
FIXTURE
  chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"
}
write_fixture_harness
printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' \
  'test -f "${MAILCLI_ROOT}/tracked.txt"' \
  'if [[ -n "${MAILCLI_TEST_GATE_SIGNAL:-}" ]]; then HELPER_PID="$(ps -o ppid= -p "$PPID" | tr -d "[:space:]")"; kill "-${MAILCLI_TEST_GATE_SIGNAL}" "${HELPER_PID}"; exit 0; fi' \
  'if [[ -n "${MAILCLI_TEST_GATE_MOVE_HEAD:-}" ]]; then git -C "${MAILCLI_TEST_GATE_MOVE_ROOT}" checkout -q --detach "${MAILCLI_TEST_GATE_MOVE_HEAD}"; fi' \
  'exit "${MAILCLI_TEST_GATE_STATUS:-0}"' >"${TEST_REPOSITORY}/scripts/tests/test-lease-fixture.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test-lease-fixture.sh"
cp "${MAILCLI_ROOT}/scripts/utils/run-staged-gate.sh" "${TEST_REPOSITORY}/scripts/utils/run-staged-gate.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"
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
stage_fixture_path scripts/tests/test-lease-fixture.sh 100755
stage_fixture_path scripts/utils/run-staged-gate.sh 100755
stage_fixture_path scripts/utils/manage-write-lease.sh 100755
INITIAL_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
INITIAL_COMMIT="$(printf 'initial\n' | git -C "${TEST_REPOSITORY}" commit-tree "${INITIAL_TREE}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${INITIAL_COMMIT}"
# One writer: a linked worktree of the same repository cannot acquire a lease.
git -C "${TEST_REPOSITORY}" worktree add -q --detach "${TEST_ROOT}/linked-worktree" "${INITIAL_COMMIT}"
if LINKED_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_ROOT}/linked-worktree" \
  "${LEASE_TOOL}" acquire 174 linked-writer tracked.txt 2>&1)"; then
  printf 'A linked worktree acquired a write lease\n' >&2
  exit 1
fi
[[ "${LINKED_OUTPUT}" == *'Write leases are acquired only in the primary worktree'* ]] || {
  printf 'Linked worktree lease failed for the wrong reason: %s\n' "${LINKED_OUTPUT}" >&2
  exit 1
}
git -C "${TEST_REPOSITORY}" worktree remove --force "${TEST_ROOT}/linked-worktree"
mkdir -p "${TEST_REPOSITORY}/docs/tasks"
printf 'board baseline\n' >"${TEST_REPOSITORY}/docs/tasks.md"
printf 'detail baseline\n' >"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
printf '/docs/tasks.md\n/docs/tasks/\n/graphify-out/\n/ignored/\n' >"${TEST_REPOSITORY}/.git/info/exclude"
mkdir -p "${TEST_REPOSITORY}/graphify-out"
printf 'graph baseline\n' >"${TEST_REPOSITORY}/graphify-out/graph.json"
mkdir -p "${TEST_REPOSITORY}/ignored/nested" "${TEST_REPOSITORY}/ignored/empty"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
printf 'nested\n' >"${TEST_REPOSITORY}/ignored/nested/keep.txt"

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

MUTATION_NUMBER=0
prove_guard_failable() {
  local COMMAND="$1" OWNER_TOKEN="$2" MESSAGE="$3" SOURCE_MESSAGE="${4:-$3}"
  local LINE MATCHES=0 MUTANT_OUTPUT
  MUTATION_NUMBER=$((MUTATION_NUMBER + 1))
  local MUTANT_ROOT="${TEST_ROOT}/mutation-${MUTATION_NUMBER}"
  mkdir -p "${MUTANT_ROOT}/scripts/utils"
  cp -R "${TEST_REPOSITORY}/.git/mailcli-write-lease" "${MUTANT_ROOT}/saved-lease"
  while IFS= read -r LINE || [[ -n "${LINE}" ]]; do
    if [[ "${LINE}" == *"fail \"${SOURCE_MESSAGE}\"" ]]; then
      printf '    :\n'
      MATCHES=$((MATCHES + 1))
    else
      printf '%s\n' "${LINE}"
    fi
  done <"${LEASE_TOOL}" >"${MUTANT_ROOT}/scripts/utils/manage-write-lease.sh"
  [[ "${MATCHES}" == 1 ]] || {
    printf 'Expected exactly one guard to mutate: %s (%s)\n' "${SOURCE_MESSAGE}" "${MATCHES}" >&2
    exit 1
  }
  chmod 755 "${MUTANT_ROOT}/scripts/utils/manage-write-lease.sh"
  MUTANT_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${MUTANT_ROOT}/scripts/utils/manage-write-lease.sh" "${COMMAND}" "${OWNER_TOKEN}" 2>&1)" || true
  [[ "${MUTANT_OUTPUT}" != *"${MESSAGE}"* ]] || {
    printf 'Negative fixture survived removal of its own guard: %s\n' "${MESSAGE}" >&2
    exit 1
  }
  # Restore only this fixture's authority state, including absence of new proof.
  rm -rf "${TEST_REPOSITORY}/.git/mailcli-write-lease"
  cp -R "${MUTANT_ROOT}/saved-lease" "${TEST_REPOSITORY}/.git/mailcli-write-lease"
}

mkdir "${TEST_ROOT}/entropy-bin"
REAL_OD="$(command -v od)"
export MAILCLI_TEST_REAL_OD="${REAL_OD}" MAILCLI_TEST_ENTROPY_RECEIPT="${TEST_ROOT}/entropy-receipt"
printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' \
  '[[ "$*" == "-v -An -N32 -tx1 /dev/urandom" ]]' \
  'printf "32-byte-urandom-read\n" >>"${MAILCLI_TEST_ENTROPY_RECEIPT}"' \
  'exec "${MAILCLI_TEST_REAL_OD}" "$@"' >"${TEST_ROOT}/entropy-bin/od"
chmod 755 "${TEST_ROOT}/entropy-bin/od"
ACQUIRE_OUTPUT="$(PATH="${TEST_ROOT}/entropy-bin:${PATH}" MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 174 test-writer tracked.txt)"
[[ "$(cat "${MAILCLI_TEST_ENTROPY_RECEIPT}")" == 32-byte-urandom-read ]]
TOKEN="$(printf '%s\n' "${ACQUIRE_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
[[ "${TOKEN}" =~ ^[0-9a-f]{64}$ ]]
LEASE_METADATA="${TEST_REPOSITORY}/.git/mailcli-write-lease"
[[ "$(cat "${LEASE_METADATA}/owner_session")" == test-writer ]]
printf 'docs/tasks\tdirectory\ndocs/tasks/done\tdirectory\n' >>"${LEASE_METADATA}/ignored_asset_fingerprints"
for UNUSED_FIELD in owner pid baseline_status reviewed_at gate_head gate_at private_task_fingerprints; do
  [[ ! -e "${LEASE_METADATA}/${UNUSED_FIELD}" ]] || {
    printf 'Unused lease metadata was persisted: %s\n' "${UNUSED_FIELD}" >&2
    exit 1
  }
done
# Private planning changes do not invalidate tracked-product authority.
printf 'private board update\n' >>"${TEST_REPOSITORY}/docs/tasks.md"
printf 'private detail update\n' >>"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
# Regenerating the ignored Graphify cache does not invalidate the lease either.
printf 'graph refresh\n' >>"${TEST_REPOSITORY}/graphify-out/graph.json"
mkdir -p "${TEST_REPOSITORY}/graphify-out/cache"
printf 'cache\n' >"${TEST_REPOSITORY}/graphify-out/cache/entry.json"

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
printf 'unstaged tracked change\n' >"${TEST_REPOSITORY}/other.txt"
if REVIEW_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${TOKEN}" 2>&1)"; then
  printf 'Review accepted an unstaged tracked change\n' >&2
  exit 1
fi
[[ "${REVIEW_OUTPUT}" == *'Unstaged tracked changes remain; stage the reviewed TASK patch exactly'* ]]
prove_guard_failable review "${TOKEN}" 'Unstaged tracked changes remain; stage the reviewed TASK patch exactly'
printf 'other\n' >"${TEST_REPOSITORY}/other.txt"
printf 'untracked\n' >"${TEST_REPOSITORY}/untracked-review"
if REVIEW_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${TOKEN}" 2>&1)"; then
  printf 'Review accepted an untracked non-ignored file\n' >&2
  exit 1
fi
[[ "${REVIEW_OUTPUT}" == *'Untracked non-ignored files remain outside the staged TASK patch'* ]]
prove_guard_failable review "${TOKEN}" 'Untracked non-ignored files remain outside the staged TASK patch'
rm "${TEST_REPOSITORY}/untracked-review"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${TOKEN}" >/dev/null

printf 'changed after review\n' >"${TEST_REPOSITORY}/tracked.txt"
stage_fixture_path tracked.txt 100644
if GATE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${TOKEN}" 2>&1)"; then
  printf 'Gate accepted changed content after review\n' >&2
  exit 1
fi
[[ "${GATE_OUTPUT}" == *'Allowed file content changed after review; review the patch again'* ]]
prove_guard_failable gate "${TOKEN}" 'Allowed file content changed after review; review the patch again'
printf 'changed\n' >"${TEST_REPOSITORY}/tracked.txt"
stage_fixture_path tracked.txt 100755
chmod 755 "${TEST_REPOSITORY}/tracked.txt"
if GATE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${TOKEN}" 2>&1)"; then
  printf 'Gate accepted a changed index mode after review\n' >&2
  exit 1
fi
[[ "${GATE_OUTPUT}" == *'Staged patch changed after review; review the patch again'* ]]
prove_guard_failable gate "${TOKEN}" 'Staged patch changed after review; review the patch again'
chmod 644 "${TEST_REPOSITORY}/tracked.txt"
stage_fixture_path tracked.txt 100644
printf 'changed\n' >"${TEST_REPOSITORY}/ignored/one.txt"
expect_ignored_scope_failure gate "${TOKEN}"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
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

MOVED_HEAD="$(printf 'concurrent fixture commit\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${INITIAL_TREE}" -p "${INITIAL_COMMIT}")"
if GATE_OUTPUT="$(MAILCLI_TEST_GATE_MOVE_ROOT="${TEST_REPOSITORY}" \
  MAILCLI_TEST_GATE_MOVE_HEAD="${MOVED_HEAD}" MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${TOKEN}" 2>&1)"; then
  printf 'Gate accepted HEAD moving while its harness ran\n' >&2
  exit 1
fi
[[ "${GATE_OUTPUT}" == *'HEAD changed while the full gate was running'* ]]
[[ ! -e "${LEASE_METADATA}/gate_patch_sha256" ]]
git -C "${TEST_REPOSITORY}" checkout -q --detach "${INITIAL_COMMIT}"
MAILCLI_TEST_GATE_MOVE_ROOT="${TEST_REPOSITORY}" MAILCLI_TEST_GATE_MOVE_HEAD="${MOVED_HEAD}" \
  prove_guard_failable gate "${TOKEN}" 'HEAD changed while the full gate was running'
git -C "${TEST_REPOSITORY}" checkout -q --detach "${INITIAL_COMMIT}"
mkdir "${TEST_ROOT}/signal-temporaries"
for SIGNAL in INT TERM; do
  SIGNAL_STATUS=0
  TMPDIR="${TEST_ROOT}/signal-temporaries" MAILCLI_TEST_GATE_SIGNAL="${SIGNAL}" \
    MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" gate "${TOKEN}" \
    >"${TEST_ROOT}/signal-output" 2>&1 || SIGNAL_STATUS=$?
  EXPECTED_SIGNAL_STATUS=130
  [[ "${SIGNAL}" != TERM ]] || EXPECTED_SIGNAL_STATUS=143
  [[ "${SIGNAL_STATUS}" == "${EXPECTED_SIGNAL_STATUS}" ]] || {
    printf '%s interruption returned %s instead of %s\n' "${SIGNAL}" "${SIGNAL_STATUS}" "${EXPECTED_SIGNAL_STATUS}" >&2
    cat "${TEST_ROOT}/signal-output" >&2
    exit 1
  }
  [[ -z "$(find "${TEST_ROOT}/signal-temporaries" -mindepth 1 -print)" ]] || {
    printf '%s interruption leaked an owned gate temporary\n' "${SIGNAL}" >&2
    exit 1
  }
  [[ ! -e "${LEASE_METADATA}/gate_patch_sha256" ]]
done
mkdir "${TEST_ROOT}/signal-mutant-temporaries"
while IFS= read -r LINE || [[ -n "${LINE}" ]]; do
  [[ "${LINE}" != "trap 'rm -rf -- \"\${TEST_ROOT}\"' EXIT" ]] || continue
  printf '%s\n' "${LINE}"
done <"${MAILCLI_ROOT}/scripts/utils/run-staged-gate.sh" >"${TEST_ROOT}/signal-mutant-helper.sh"
chmod 755 "${TEST_ROOT}/signal-mutant-helper.sh"
SIGNAL_STATUS=0
TMPDIR="${TEST_ROOT}/signal-mutant-temporaries" MAILCLI_TEST_GATE_SIGNAL=TERM \
  "${TEST_ROOT}/signal-mutant-helper.sh" "${TEST_REPOSITORY}" "${INITIAL_COMMIT}" \
  "$(git -C "${TEST_REPOSITORY}" write-tree)" --checks scripts/tests/test-lease-fixture.sh \
  >"${TEST_ROOT}/signal-mutant-output" 2>&1 || SIGNAL_STATUS=$?
[[ "${SIGNAL_STATUS}" == 143 && -n "$(find "${TEST_ROOT}/signal-mutant-temporaries" -mindepth 1 -print)" ]] || {
  printf 'Interruption cleanup fixture did not fail after removal of its cleanup trap\n' >&2
  cat "${TEST_ROOT}/signal-mutant-output" >&2
  exit 1
}
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${TOKEN}" >/dev/null
TASK_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
TASK_COMMIT="$(printf 'TASK 174: coordination fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${TASK_TREE}" -p "${INITIAL_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${TASK_COMMIT}"
printf 'changed\n' >"${TEST_REPOSITORY}/ignored/one.txt"
expect_ignored_scope_failure release "${TOKEN}"
printf 'first\n' >"${TEST_REPOSITORY}/ignored/one.txt"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${TOKEN}" >"${TEST_ROOT}/singleton-release"
grep -Fq 'task_commit_subject=TASK 174: coordination fixture' "${TEST_ROOT}/singleton-release"
grep -Fq 'tracked.txt' "${TEST_ROOT}/singleton-release"
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

DIRECTORY_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175 directory-owner scripts/tests 2>&1)" && {
  printf 'Acquire accepted a tracked directory instead of exact files\n' >&2
  exit 1
}
[[ "${DIRECTORY_OUTPUT}" == *'Lease paths must name tracked files, not a tracked directory: scripts/tests'* ]]
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

ABORT_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175 abort-owner other.txt)"
ABORT_TOKEN="$(printf '%s\n' "${ABORT_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
[[ "${ABORT_TOKEN}" =~ ^[0-9a-f]{64}$ && "${ABORT_TOKEN}" != "${TOKEN}" ]]
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

HARNESS_ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 176 harness-writer scripts/tests/test.sh)"
HARNESS_TOKEN="$(printf '%s\n' "${HARNESS_ACQUIRE_OUTPUT}" |
  sed -n 's/^write_lease_token=//p')"
[[ -n "${HARNESS_TOKEN}" ]]
sed 's/baseline-harness/patched-harness/' "${TEST_REPOSITORY}/scripts/tests/test.sh" >"${TEST_ROOT}/patched-harness"
cp "${TEST_ROOT}/patched-harness" "${TEST_REPOSITORY}/scripts/tests/test.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"
stage_fixture_path scripts/tests/test.sh 100755
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${HARNESS_TOKEN}" >/dev/null
GATE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${HARNESS_TOKEN}" 2>&1)"
printf '%s\n' "${GATE_OUTPUT}" | grep -Fq 'patched-harness' || {
  printf 'Gate did not run the staged harness\n' >&2
  exit 1
}
printf '%s\n' "${GATE_OUTPUT}" | grep -Fq 'gate_harness=staged' || {
  printf 'Gate did not report staged harness selection\n' >&2
  exit 1
}
TASK_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
TASK_COMMIT="$(printf 'TASK 176: baseline harness fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${TASK_TREE}" -p "${TASK_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${TASK_COMMIT}"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${HARNESS_TOKEN}" >/dev/null
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

printf '%s\n' \
  '# MailCLI Tasks' \
  '## Active' \
  '## Queue' \
  '- [ ] 174 First fixture -> tasks/174-detail.md' \
  '- [ ] 175 Second fixture -> tasks/175-second.md' \
  '## Blocked' \
  '## Done' >"${TEST_REPOSITORY}/docs/tasks.md"
printf '# TASK 174: First fixture\n' >"${TEST_REPOSITORY}/docs/tasks/174-detail.md"
printf '# TASK 175: Second fixture\n' >"${TEST_REPOSITORY}/docs/tasks/175-second.md"

for INVALID_IDS in '174,174' '174,' ',174' '17,175' '174, 175' '174,999'; do
  if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${LEASE_TOOL}" acquire "${INVALID_IDS}" group-owner tracked.txt >/dev/null 2>&1; then
    printf 'Acquire accepted invalid or unapproved group: %s\n' "${INVALID_IDS}" >&2
    exit 1
  fi
  [[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]
done

expect_group_rejection() {
  local EXPECTED_MESSAGE="$1"
  local OUTPUT
  if OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${LEASE_TOOL}" acquire 174,175 group-owner tracked.txt 2>&1)"; then
    printf 'Acquire accepted an invalid board/detail group\n' >&2
    exit 1
  fi
  [[ "${OUTPUT}" == *"${EXPECTED_MESSAGE}"* ]]
  [[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]
}
cp "${TEST_REPOSITORY}/docs/tasks.md" "${TEST_ROOT}/group-board"
printf '%s\n' '- [x] 175 Duplicate fixture -> tasks/done/175-second.md' >>"${TEST_REPOSITORY}/docs/tasks.md"
expect_group_rejection 'Grouped TASK 175 has duplicate board entries'
cp "${TEST_ROOT}/group-board" "${TEST_REPOSITORY}/docs/tasks.md"
for CLOSED_STATE in '!' 'x'; do
  sed "s/- \\[ \\] 175 /- [${CLOSED_STATE}] 175 /" \
    "${TEST_ROOT}/group-board" >"${TEST_REPOSITORY}/docs/tasks.md"
  expect_group_rejection 'Grouped TASK 175 must have one open local board entry'
done
cp "${TEST_ROOT}/group-board" "${TEST_REPOSITORY}/docs/tasks.md"
mv "${TEST_REPOSITORY}/docs/tasks/175-second.md" "${TEST_ROOT}/group-detail"
expect_group_rejection 'Grouped TASK 175 lacks a regular detail file'
ln -s "${TEST_ROOT}/group-detail" "${TEST_REPOSITORY}/docs/tasks/175-second.md"
expect_group_rejection 'Grouped TASK 175 lacks a regular detail file'
rm "${TEST_REPOSITORY}/docs/tasks/175-second.md"
printf '# TASK 176: Wrong member\n' >"${TEST_REPOSITORY}/docs/tasks/175-second.md"
expect_group_rejection 'Grouped TASK 175 detail has the wrong ID'
mv "${TEST_ROOT}/group-detail" "${TEST_REPOSITORY}/docs/tasks/175-second.md"

# Seed an independent marker-bearing baseline for the grouping cases.
write_fixture_harness
stage_fixture_path scripts/tests/test.sh 100755
GROUP_BASELINE_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
GROUP_BASELINE="$(printf 'group baseline\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${GROUP_BASELINE_TREE}" -p "${TASK_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${GROUP_BASELINE}"
GROUP_ACQUIRE="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 175,174 group-owner tracked.txt other.txt)"
GROUP_TOKEN="$(printf '%s\n' "${GROUP_ACQUIRE}" | sed -n 's/^write_lease_token=//p')"
[[ "${GROUP_ACQUIRE}" == *'write_lease_task=174,175'* ]]
printf 'group change\n' >"${TEST_REPOSITORY}/tracked.txt"
printf 'other group change\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path tracked.txt 100644
stage_fixture_path other.txt 100644
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${GROUP_TOKEN}" >/dev/null
GROUP_GATE_STATUS=0
MAILCLI_TEST_GATE_STATUS=23 MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${GROUP_TOKEN}" >/dev/null 2>&1 || GROUP_GATE_STATUS=$?
[[ "${GROUP_GATE_STATUS}" == 23 ]]
[[ ! -e "${TEST_REPOSITORY}/.git/mailcli-write-lease/gate_patch_sha256" ]]
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${GROUP_TOKEN}" >/dev/null
GROUP_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
SUBJECT_MUTATION_PROVED=false
for BAD_SUBJECT in \
  'TASK 174: missing member' \
  'TASK 174, 175, 176: extra member' \
  'TASK 174, 174, 175: duplicate member' \
  'TASK 175, 174: wrong order' \
  'TASK 174,175: noncanonical separator' \
  'TASK 174, 175: '; do
  BAD_COMMIT="$(printf '%s\n' "${BAD_SUBJECT}" |
    git -C "${TEST_REPOSITORY}" commit-tree "${GROUP_TREE}" -p "${GROUP_BASELINE}")"
  git -C "${TEST_REPOSITORY}" checkout -q --detach "${BAD_COMMIT}"
  if RELEASE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${LEASE_TOOL}" release "${GROUP_TOKEN}" 2>&1)"; then
    printf 'Release accepted the wrong group subject: %s\n' "${BAD_SUBJECT}" >&2
    exit 1
  fi
  [[ "${RELEASE_OUTPUT}" == *'Commit subject must start with TASK 174, 175:'* ]]
  if [[ "${SUBJECT_MUTATION_PROVED}" == false ]]; then
    prove_guard_failable release "${GROUP_TOKEN}" 'Commit subject must start with TASK 174, 175:' \
      'Commit subject must start with ${SUBJECT_PREFIX}and describe the change'
    SUBJECT_MUTATION_PROVED=true
  fi
  [[ -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]
done
GROUP_COMMIT="$(printf 'TASK 174, 175: grouped fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${GROUP_TREE}" -p "${GROUP_BASELINE}")"
printf 'different committed patch\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
DIFFERENT_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
DIFFERENT_COMMIT="$(printf 'TASK 174, 175: different patch\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${DIFFERENT_TREE}" -p "${GROUP_BASELINE}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${DIFFERENT_COMMIT}"
if RELEASE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${GROUP_TOKEN}" 2>&1)"; then
  printf 'Release accepted a commit different from its gated patch\n' >&2
  exit 1
fi
[[ "${RELEASE_OUTPUT}" == *'Committed patch differs from the exact patch that passed the full gate'* ]]
prove_guard_failable release "${GROUP_TOKEN}" 'Committed patch differs from the exact patch that passed the full gate'
INTERMEDIATE_COMMIT="$(printf 'intermediate fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${GROUP_TREE}" -p "${GROUP_BASELINE}")"
FOREIGN_PARENT_COMMIT="$(printf 'TASK 174, 175: wrong parent\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${GROUP_TREE}" -p "${INITIAL_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${FOREIGN_PARENT_COMMIT}"
if RELEASE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${GROUP_TOKEN}" 2>&1)"; then
  printf 'Release accepted a commit based on another parent\n' >&2
  exit 1
fi
[[ "${RELEASE_OUTPUT}" == *'Lease requires exactly one TASK commit whose parent is the acquired HEAD'* ]]
prove_guard_failable release "${GROUP_TOKEN}" 'Lease requires exactly one TASK commit whose parent is the acquired HEAD'
WRONG_PARENT_COMMIT="$(printf 'TASK 174, 175: two commits\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${GROUP_TREE}" -p "${INTERMEDIATE_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${WRONG_PARENT_COMMIT}"
if RELEASE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${GROUP_TOKEN}" 2>&1)"; then
  printf 'Release accepted multiple commits with the same final tree\n' >&2
  exit 1
fi
[[ "${RELEASE_OUTPUT}" == *'Lease requires exactly one TASK commit whose parent is the acquired HEAD'* ]]
prove_guard_failable release "${GROUP_TOKEN}" 'Lease requires exactly one TASK commit whose parent is the acquired HEAD'
git -C "${TEST_REPOSITORY}" checkout -q --detach "${GROUP_COMMIT}"
printf 'untracked post-commit change\n' >"${TEST_REPOSITORY}/dirty-after-commit"
if RELEASE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${GROUP_TOKEN}" 2>&1)"; then
  printf 'Release accepted a dirty worktree after the exact gated commit\n' >&2
  exit 1
fi
[[ "${RELEASE_OUTPUT}" == *'Worktree is not clean after the TASK commit'* ]]
prove_guard_failable release "${GROUP_TOKEN}" 'Worktree is not clean after the TASK commit'
rm "${TEST_REPOSITORY}/dirty-after-commit"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" release "${GROUP_TOKEN}" >"${TEST_ROOT}/group-release"
grep -Fq 'task_commit_subject=TASK 174, 175: grouped fixture' "${TEST_ROOT}/group-release"
grep -Fq 'other.txt' "${TEST_ROOT}/group-release"
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

REVIEW_PATHS=(tracked.txt)
for ((INDEX = 1; INDEX <= 40; INDEX++)); do
  REVIEW_PATHS+=("review-long-file-$(printf '%0180d' "${INDEX}").txt")
done
LARGE_ACQUIRE="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 177 review-output-owner "${REVIEW_PATHS[@]}")"
LARGE_TOKEN="$(printf '%s\n' "${LARGE_ACQUIRE}" | sed -n 's/^write_lease_token=//p')"
awk 'BEGIN { for (line = 1; line <= 4000; line++) printf "review-line-%04d\n", line }' \
  >"${TEST_REPOSITORY}/tracked.txt"
stage_fixture_path tracked.txt 100644
for REVIEW_PATH in "${REVIEW_PATHS[@]:1}"; do
  printf 'large-review-path\n' >"${TEST_REPOSITORY}/${REVIEW_PATH}"
  stage_fixture_path "${REVIEW_PATH}" 100644
done
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" review "${LARGE_TOKEN}" \
  >"${TEST_ROOT}/default-review"
[[ "$(wc -c <"${TEST_ROOT}/default-review")" -le 3072 ]]
grep -Fxq 'reviewed_paths_count=41' "${TEST_ROOT}/default-review"
grep -Fxq 'reviewed_paths_omitted=33' "${TEST_ROOT}/default-review"
! grep -Fq '+review-line-' "${TEST_ROOT}/default-review"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" review "${LARGE_TOKEN}" --diff \
  >"${TEST_ROOT}/full-review"
grep -Fxq '+review-line-0001' "${TEST_ROOT}/full-review"
grep -Fxq '+review-line-4000' "${TEST_ROOT}/full-review"
for REVIEW_PATH in "${REVIEW_PATHS[@]}"; do grep -Fxq "  ${REVIEW_PATH}" "${TEST_ROOT}/full-review"; done
[[ "$(sed -n 's/^reviewed_patch_sha256=//p' "${TEST_ROOT}/default-review")" == \
  "$(sed -n 's/^reviewed_patch_sha256=//p' "${TEST_ROOT}/full-review")" ]]
git -C "${TEST_REPOSITORY}" read-tree --reset -u "${GROUP_COMMIT}"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" abort "${LARGE_TOKEN}" >/dev/null

# extend adds unchanged tracked paths to an active lease; review then accepts them.
EXTEND_ACQUIRE="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" acquire 178 extend-owner tracked.txt)"
EXTEND_TOKEN="$(printf '%s\n' "${EXTEND_ACQUIRE}" | sed -n 's/^write_lease_token=//p')"
expect_extend_rejection() {
  local EXPECTED_MESSAGE="$1" EXTEND_OWNER_TOKEN="$2"
  shift 2
  local OUTPUT
  if OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
    "${LEASE_TOOL}" extend "${EXTEND_OWNER_TOKEN}" "$@" 2>&1)"; then
    printf 'Extend accepted an invalid path set: %s\n' "$*" >&2
    exit 1
  fi
  [[ "${OUTPUT}" == *"${EXPECTED_MESSAGE}"* ]] || {
    printf 'Extend failed for the wrong reason: %s\n' "${OUTPUT}" >&2
    exit 1
  }
}
expect_extend_rejection 'Write lease token does not match the active owner' \
  "$(printf '%064d' 0)" other.txt
expect_extend_rejection 'Path is already leased: tracked.txt' "${EXTEND_TOKEN}" tracked.txt
expect_extend_rejection 'Lease paths must name tracked files, not a tracked directory: scripts' "${EXTEND_TOKEN}" scripts
printf 'changed before lease\n' >>"${TEST_REPOSITORY}/other.txt"
expect_extend_rejection 'Path changed before it was leased: other.txt' "${EXTEND_TOKEN}" other.txt
git -C "${TEST_REPOSITORY}" checkout -q -- other.txt
grep -Fxq other.txt "${LEASE_METADATA}/allowed_paths" && {
  printf 'Rejected extend changed the lease scope\n' >&2
  exit 1
}
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" extend "${EXTEND_TOKEN}" other.txt >"${TEST_ROOT}/extend-output"
grep -Fxq 'write_lease_extended=other.txt' "${TEST_ROOT}/extend-output"
grep -Fxq other.txt "${LEASE_METADATA}/allowed_paths"
printf 'extended change\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" review "${EXTEND_TOKEN}" >"${TEST_ROOT}/extend-review"
grep -Fq other.txt "${TEST_ROOT}/extend-review"

# precommit (run by the local pre-commit hook) allows a commit only for the exact gated patch.
expect_precommit_refusal() {
  local EXPECTED_MESSAGE="$1" OUTPUT
  if OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" precommit 2>&1)"; then
    printf 'precommit accepted a commit: %s\n' "${EXPECTED_MESSAGE}" >&2
    exit 1
  fi
  [[ "${OUTPUT}" == *"${EXPECTED_MESSAGE}"* ]] || {
    printf 'precommit refused for the wrong reason: %s\n' "${OUTPUT}" >&2
    exit 1
  }
}
expect_precommit_refusal 'the active write lease has no passed gate'
MAILCLI_TEST_GATE_STATUS=1 MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" \
  "${LEASE_TOOL}" gate "${EXTEND_TOKEN}" >/dev/null 2>&1 || true
expect_precommit_refusal 'the active write lease has no passed gate'
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" gate "${EXTEND_TOKEN}" >/dev/null
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" precommit
printf 'changed after the gate\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
expect_precommit_refusal 'the staged patch differs from the patch that passed the gate'
git -C "${TEST_REPOSITORY}" read-tree --reset -u "${GROUP_COMMIT}"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" abort "${EXTEND_TOKEN}" >/dev/null
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY_ALIAS}" "${LEASE_TOOL}" precommit

printf 'Write coordination passed: ownership, path and asset scope, failure-preserving gate, tested commit identity, and exact grouped TASK membership\n'

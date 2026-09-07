#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LEASE_TOOL="${MAILCLI_ROOT}/scripts/utils/manage-write-lease.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-write-coordination.XXXXXX")"
TEST_REPOSITORY="${TEST_ROOT}/repo"

cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-write-coordination."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

mkdir -p "${TEST_REPOSITORY}/scripts/tests"
git -C "${TEST_REPOSITORY}" init -q -b main
git -C "${TEST_REPOSITORY}" config user.name "MailCLI Test"
git -C "${TEST_REPOSITORY}" config user.email "mailcli-test@example.invalid"
printf 'original\n' >"${TEST_REPOSITORY}/tracked.txt"
printf 'other\n' >"${TEST_REPOSITORY}/other.txt"
printf '%s\n' '#!/usr/bin/env bash' "exit \"\${MAILCLI_TEST_GATE_STATUS:-0}\"" >"${TEST_REPOSITORY}/scripts/tests/test.sh"
chmod 755 "${TEST_REPOSITORY}/scripts/tests/test.sh"

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
INITIAL_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
INITIAL_COMMIT="$(printf 'initial\n' | git -C "${TEST_REPOSITORY}" commit-tree "${INITIAL_TREE}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${INITIAL_COMMIT}"

ACQUIRE_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" acquire 174 test-writer tracked.txt)"
TOKEN="$(printf '%s\n' "${ACQUIRE_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
[[ -n "${TOKEN}" ]]

if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" acquire 175 competing-writer other.txt >/dev/null 2>&1; then
  printf 'A second writer acquired the same worktree lease\n' >&2
  exit 1
fi

printf 'unauthorized\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" review "${TOKEN}" >/dev/null 2>&1; then
  printf 'Review accepted a staged path outside the lease allowlist\n' >&2
  exit 1
fi

printf 'other\n' >"${TEST_REPOSITORY}/other.txt"
stage_fixture_path other.txt 100644
printf 'changed\n' >"${TEST_REPOSITORY}/tracked.txt"
stage_fixture_path tracked.txt 100644
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" review "${TOKEN}" >/dev/null

GATE_STATUS=0
MAILCLI_TEST_GATE_STATUS=23 MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" gate "${TOKEN}" >/dev/null 2>&1 || GATE_STATUS=$?
if [[ "${GATE_STATUS}" -ne 23 ]]; then
  printf 'Failing full gate status was not preserved: %s\n' "${GATE_STATUS}" >&2
  exit 1
fi
if [[ -e "${TEST_REPOSITORY}/.git/mailcli-write-lease/gate_patch_sha256" ]]; then
  printf 'Failing full gate recorded commit evidence\n' >&2
  exit 1
fi

MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" gate "${TOKEN}" >/dev/null
TASK_TREE="$(git -C "${TEST_REPOSITORY}" write-tree)"
TASK_COMMIT="$(printf 'TASK 174: coordination fixture\n' |
  git -C "${TEST_REPOSITORY}" commit-tree "${TASK_TREE}" -p "${INITIAL_COMMIT}")"
git -C "${TEST_REPOSITORY}" checkout -q --detach "${TASK_COMMIT}"
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" release "${TOKEN}" >/dev/null
[[ ! -d "${TEST_REPOSITORY}/.git/mailcli-write-lease" ]]

ABORT_OUTPUT="$(MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" acquire 175 abort-owner other.txt)"
ABORT_TOKEN="$(printf '%s\n' "${ABORT_OUTPUT}" | sed -n 's/^write_lease_token=//p')"
if MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" abort wrong-token >/dev/null 2>&1; then
  printf 'Wrong owner token aborted the active write lease\n' >&2
  exit 1
fi
MAILCLI_WRITE_ROOT="${TEST_REPOSITORY}" \
  "${LEASE_TOOL}" abort "${ABORT_TOKEN}" >/dev/null

printf 'Write coordination passed: one writer, exact path scope, failure-preserving gate, and tested commit identity\n'

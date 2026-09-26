#!/usr/bin/env bash
set -euo pipefail

ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
source "${ROOT}/scripts/utils/check-go-toolchain.sh"
source "${ROOT}/scripts/utils/manage-full-proof.sh"
check_go_toolchain "${ROOT}"
go test -mod=readonly -vet=off -count=1 "${ROOT}/scripts/utils/select-gate-packages"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-fast-gate-test.XXXXXX")"
cleanup() {
  local STATUS="$?"
  if [[ "${STATUS}" != 0 ]]; then
    for LOG in "${TEST_ROOT}/"*.log; do
      [[ ! -f "${LOG}" ]] || { printf 'Failed fixture evidence: %s\n' "${LOG}" >&2; tail -n 30 "${LOG}" >&2; }
    done
  fi
  rm -rf -- "${TEST_ROOT}"
}
trap cleanup EXIT
REPOSITORY="${TEST_ROOT}/repository"
mkdir -p "${REPOSITORY}/scripts/utils" "${REPOSITORY}/scripts/tests" \
  "${REPOSITORY}/leaf" "${REPOSITORY}/consumer" "${REPOSITORY}/unrelated"
cp -R "${ROOT}/scripts/utils/select-gate-packages" "${REPOSITORY}/scripts/utils/"
cp "${ROOT}/.golangci.yml" "${REPOSITORY}/.golangci.yml"
cp "${ROOT}/scripts/tests/"test*.sh "${REPOSITORY}/scripts/tests/"
for NAME in run-fast-gate.sh run-staged-gate.sh manage-full-proof.sh manage-write-lease.sh check-go-toolchain.sh; do
  cp "${ROOT}/scripts/utils/${NAME}" "${REPOSITORY}/scripts/utils/${NAME}"
done
printf 'module gatefixture\n\ngo %s\n' "${GOTOOLCHAIN#go}" >"${REPOSITORY}/go.mod"
printf 'package leaf\n\nfunc Value() int { return 1 }\n' >"${REPOSITORY}/leaf/value.go"
printf 'package unrelated\n\nfunc Value() int { return 1 }\n' >"${REPOSITORY}/unrelated/value.go"
printf 'package consumer\n\nimport "gatefixture/leaf"\n\nfunc Value() int { return leaf.Value() }\n' >"${REPOSITORY}/consumer/value.go"
printf 'package consumer\n\nimport "testing"\n\nfunc TestLeaf(t *testing.T) { if Value() != 1 { t.Fatal("changed leaf reached unchanged consumer") } }\n' \
  >"${REPOSITORY}/consumer/value_test.go"
gofmt -w "${REPOSITORY}/consumer/value_test.go"
git -C "${REPOSITORY}" init -q -b main
git -C "${REPOSITORY}" config user.name 'MailCLI Test'
git -C "${REPOSITORY}" config user.email 'mailcli-test@example.invalid'

stage_path() {
  local PATH_NAME="$1"
  local MODE=100644
  [[ ! -x "${REPOSITORY}/${PATH_NAME}" ]] || MODE=100755
  local BLOB
  BLOB="$(git -C "${REPOSITORY}" hash-object -w "${REPOSITORY}/${PATH_NAME}")"
  git -C "${REPOSITORY}" update-index --add --cacheinfo "${MODE},${BLOB},${PATH_NAME}"
}
while IFS= read -r -d '' PATH_NAME; do stage_path "${PATH_NAME}"; done \
  < <(git -C "${REPOSITORY}" ls-files --others --exclude-standard -z)
BASELINE_TREE="$(git -C "${REPOSITORY}" write-tree)"
BASELINE_HEAD="$(printf 'fast gate fixture\n' | git -C "${REPOSITORY}" commit-tree "${BASELINE_TREE}")"
git -C "${REPOSITORY}" checkout -q --detach "${BASELINE_HEAD}"
ACQUIRE="$(MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" acquire 506 fixture leaf/value.go)"
TOKEN="$(printf '%s\n' "${ACQUIRE}" | sed -n 's/^write_lease_token=//p')"
RECEIPT="${REPOSITORY}/.git/mailcli-write-lease/lint_identity"

printf 'package leaf\n\nfunc Value() int { return 2 }\n' >"${REPOSITORY}/leaf/value.go"
stage_path leaf/value.go
STATUS=0
"${ROOT}/scripts/utils/run-fast-gate.sh" "${REPOSITORY}" "${BASELINE_HEAD}" \
  "$(git -C "${REPOSITORY}" write-tree)" --fast "${RECEIPT}" >"${TEST_ROOT}/dependency.log" 2>&1 || STATUS=$?
[[ "${STATUS}" != 0 ]]
grep -q 'changed leaf reached unchanged consumer' "${TEST_ROOT}/dependency.log"
grep -qx 'selected_package=gatefixture/consumer' "${TEST_ROOT}/dependency.log"
! grep -qx 'selected_package=gatefixture/unrelated' "${TEST_ROOT}/dependency.log"

printf 'package leaf\n\nimport "io"\n\nfunc Value() int { return 1 }\nfunc Close(c io.Closer) { c.Close() }\n' >"${REPOSITORY}/leaf/value.go"
gofmt -w "${REPOSITORY}/leaf/value.go"
stage_path leaf/value.go
STATUS=0
MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" review "${TOKEN}" \
  >"${TEST_ROOT}/review.log" 2>&1 || STATUS=$?
[[ "${STATUS}" != 0 && ! -e "${REPOSITORY}/.git/mailcli-write-lease/reviewed_patch_sha256" ]]
grep -q '(errcheck)' "${TEST_ROOT}/review.log"

printf 'package leaf\n\nimport "io"\n\nfunc Value() int { return 1 }\nfunc Close(c io.Closer) error { return c.Close() }\n' >"${REPOSITORY}/leaf/value.go"
gofmt -w "${REPOSITORY}/leaf/value.go"
STATUS=0
"${ROOT}/scripts/utils/run-fast-gate.sh" "${REPOSITORY}" "${BASELINE_HEAD}" \
  "$(git -C "${REPOSITORY}" write-tree)" --lint-only "${RECEIPT}" >"${TEST_ROOT}/divergence.log" 2>&1 || STATUS=$?
[[ "${STATUS}" != 0 ]]
grep -q '(errcheck)' "${TEST_ROOT}/divergence.log"
stage_path leaf/value.go
REVIEW_STARTED="${SECONDS}"
MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" review "${TOKEN}" >"${TEST_ROOT}/valid-review.log"
REVIEW_SECONDS=$((SECONDS - REVIEW_STARTED))
"${ROOT}/scripts/utils/run-fast-gate.sh" "${REPOSITORY}" "${BASELINE_HEAD}" \
  "$(git -C "${REPOSITORY}" write-tree)" --fast "${RECEIPT}" >"${TEST_ROOT}/valid-gate.log"
grep -qx 'lint_reused=true' "${TEST_ROOT}/valid-gate.log"
grep -qx 'gate_tier=fast' "${TEST_ROOT}/valid-gate.log"
grep -qx 'full_gate=deferred' "${TEST_ROOT}/valid-gate.log"
! grep -q 'vulnerability_check=passed' "${TEST_ROOT}/valid-gate.log"
git -C "${REPOSITORY}" read-tree --reset -u "${BASELINE_TREE}"
MAILCLI_WRITE_ROOT="${REPOSITORY}" "${REPOSITORY}/scripts/utils/manage-write-lease.sh" abort "${TOKEN}" >/dev/null

for STATE in missing offline stale; do
  if [[ "${STATE}" != missing ]]; then
    printf 'old full verification\n' >"${REPOSITORY}/.git/mailcli-full-proof.log"
    LOG_HASH="$(shasum -a 256 "${REPOSITORY}/.git/mailcli-full-proof.log" | awk '{print $1}')"
    SCAN=passed
    [[ "${STATE}" != offline ]] || SCAN=skipped_offline
    jq -n --arg receipt "${LOG_HASH}" --arg scan "${SCAN}" \
      '{schema:1,identity:"obsolete",full_gate:"passed",vulnerability_check:$scan,receipt_sha256:$receipt}' \
      >"${REPOSITORY}/.git/mailcli-full-proof.json"
  fi
  STATUS=0
  "${ROOT}/scripts/utils/manage-full-proof.sh" check "${REPOSITORY}" >"${TEST_ROOT}/proof.log" 2>&1 || STATUS=$?
  [[ "${STATUS}" != 0 ]]
  grep -q 'Full proof is missing, incomplete or stale' "${TEST_ROOT}/proof.log"
done

# Receipt-policy unit inputs do not execute or certify a real full scan.
UNIT_LOG="${TEST_ROOT}/receipt-unit.log"
UNIT_PROOF="${TEST_ROOT}/receipt-unit.json"
CORE_HASH="$(shasum -a 256 "${ROOT}/scripts/tests/test.sh" | awk '{print $1}')"
UNIT_IDENTITY="${CORE_HASH}"
printf 'gate_tier=full\nvulnerability_check=passed\nshell_test_receipt=%s\tcore-checks\n' "${CORE_HASH}" >"${UNIT_LOG}"
UNIT_LOG_HASH="$(shasum -a 256 "${UNIT_LOG}" | awk '{print $1}')"
jq -n --arg identity "${UNIT_IDENTITY}" --arg receipt "${UNIT_LOG_HASH}" --arg head "${BASELINE_HEAD}" --arg tree "${BASELINE_TREE}" \
  '{schema:1,identity:$identity,head:$head,tree:$tree,full_gate:"passed",vulnerability_check:"passed",receipt_sha256:$receipt}' >"${UNIT_PROOF}"
validate_full_receipt "${UNIT_PROOF}" "${UNIT_LOG}" "${UNIT_IDENTITY}" "${BASELINE_HEAD}" "${BASELINE_TREE}"
for MISMATCH in identity head tree; do
  EXPECTED_IDENTITY="${UNIT_IDENTITY}" EXPECTED_HEAD="${BASELINE_HEAD}" EXPECTED_TREE="${BASELINE_TREE}"
  case "${MISMATCH}" in
    identity) EXPECTED_IDENTITY=stale ;;
    head) EXPECTED_HEAD=stale ;;
    tree) EXPECTED_TREE=stale ;;
  esac
  if validate_full_receipt "${UNIT_PROOF}" "${UNIT_LOG}" "${EXPECTED_IDENTITY}" "${EXPECTED_HEAD}" "${EXPECTED_TREE}"; then
    printf 'Receipt validator accepted changed %s\n' "${MISMATCH}" >&2; exit 1
  fi
done
printf 'tampered log\n' >>"${UNIT_LOG}"
if validate_full_receipt "${UNIT_PROOF}" "${UNIT_LOG}" "${UNIT_IDENTITY}" "${BASELINE_HEAD}" "${BASELINE_TREE}"; then
  printf 'Receipt validator accepted tampered log bytes\n' >&2; exit 1
fi
printf '\n// dirty source\n' >>"${REPOSITORY}/leaf/value.go"
STATUS=0
"${ROOT}/scripts/utils/manage-full-proof.sh" check "${REPOSITORY}" >"${TEST_ROOT}/dirty.log" 2>&1 || STATUS=$?
[[ "${STATUS}" != 0 ]]
grep -q 'requires a clean HEAD' "${TEST_ROOT}/dirty.log"
printf 'Fast gate passed: real reverse consumer failure, staged lint refusal, reusable exact-tree lint, and stale/dirty/incomplete full-proof refusal\n'
printf 'review_fixture_seconds=%s\n' "${REVIEW_SECONDS}"

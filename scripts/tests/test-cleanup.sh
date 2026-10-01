#!/usr/bin/env bash
set -euo pipefail

PRODUCT_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)}"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-cleanup-test.XXXXXX")"
TEST_ROOT="$(CDPATH= cd -P "${TEST_ROOT}" && pwd)"
FIXTURE=''
cleanup_test_root() {
  [[ -z "${FIXTURE}" || ! -d "${FIXTURE}" ]] || chmod u+w "${FIXTURE}"
  rm -rf -- "${TEST_ROOT}"
}
trap cleanup_test_root EXIT
fail() { printf 'Cleanup regression failed: %s\n' "$1" >&2; exit 1; }
[[ -x "${PRODUCT_ROOT}/cleanup.sh" ]] || fail 'cleanup.sh is missing or not executable'
bash -n "${PRODUCT_ROOT}/cleanup.sh"

new_repository() {
  FIXTURE="${TEST_ROOT}/$1"
  mkdir "${FIXTURE}"
  git -C "${FIXTURE}" init -q
  cp "${PRODUCT_ROOT}/cleanup.sh" "${FIXTURE}/cleanup.sh"
}

expect_refusal() {
  local MESSAGE="$1" SCRIPT="${2:-${FIXTURE}/cleanup.sh}" STATUS=0
  "${SCRIPT}" >"${TEST_ROOT}/refusal.log" 2>&1 || STATUS=$?
  [[ "${STATUS}" == 1 ]] || fail "refusal returned ${STATUS} instead of 1"
  grep -Fq "${MESSAGE}" "${TEST_ROOT}/refusal.log" || fail "missing refusal: ${MESSAGE}"
  [[ -f "${FIXTURE}/bin/sentinel" ]] || fail 'preflight refusal deleted an earlier valid target'
  if grep -q '^Deleted:' "${TEST_ROOT}/refusal.log"; then fail 'refused cleanup reported deletion'; fi
}

reported_kib() {
  awk -v prefix="$1" 'index($0, prefix) == 1 {
    count++; value = $(NF - 1); unit = $NF
    if (unit == "KiB") factor = 1
    else if (unit == "MiB") factor = 1024
    else if (unit == "GiB") factor = 1048576
    else exit 1
    printf "%.3f\n", value * factor
  } END { if (count != 1) exit 1 }' "$2"
}

new_repository 'caller checkout'
mkdir "${FIXTURE}/bin"
printf 'caller state\n' >"${FIXTURE}/bin/sentinel"
CALLER="${FIXTURE}"
new_repository 'checkout with spaces'
mkdir -p "${FIXTURE}/bin" "${FIXTURE}/dist" "${FIXTURE}/graphify-out" "${FIXTURE}/.coverage" \
  "${FIXTURE}/docs/tasks" "${FIXTURE}/internal" "${FIXTURE}/drafts"
dd if=/dev/zero of="${FIXTURE}/bin/mailcli" bs=1024 count=2048 2>/dev/null
for FILE in dist/package graphify-out/graph.json .coverage/report coverage.out coverage-unit.out \
  mailstore.test cpu.prof heap.pprof runtime.trace 'space profile.prof'; do
  printf 'generated output\n' >"${FIXTURE}/${FILE}"
done
mkdir "${FIXTURE}/dist/nested"
printf 'generated output\n' >"${FIXTURE}/dist/nested/file with spaces"
for FILE in docs/tasks/private.md internal/source.go internal/fixture.prof drafts/claim.json \
  .env evidence.log retained.backup; do
  printf 'retained state\n' >"${FIXTURE}/${FILE}"
done
PROTECTED=("${FIXTURE}/cleanup.sh" "${FIXTURE}/.git/HEAD" "${FIXTURE}/.git/config" \
  "${FIXTURE}/docs/tasks/private.md" "${FIXTURE}/internal/source.go" "${FIXTURE}/internal/fixture.prof" \
  "${FIXTURE}/drafts/claim.json" "${FIXTURE}/.env" "${FIXTURE}/evidence.log" "${FIXTURE}/retained.backup" \
  "${CALLER}/bin/sentinel")
PROTECTED_BEFORE="$(shasum -a 256 "${PROTECTED[@]}")"
BEFORE="$(du -sk "${FIXTURE}" | awk '{print $1}')"
(cd "${CALLER}" && GIT_DIR="${CALLER}/.git" GIT_WORK_TREE="${CALLER}" \
  GIT_INDEX_FILE="${CALLER}/.git/index" "${FIXTURE}/cleanup.sh") >"${TEST_ROOT}/success.log"
AFTER="$(du -sk "${FIXTURE}" | awk '{print $1}')"
[[ "$(shasum -a 256 "${PROTECTED[@]}")" == "${PROTECTED_BEFORE}" ]] || fail 'protected state changed'
for FILE in bin dist graphify-out .coverage coverage.out coverage-unit.out mailstore.test cpu.prof \
  heap.pprof runtime.trace 'space profile.prof'; do
  [[ ! -e "${FIXTURE}/${FILE}" ]] || fail "generated target remains: ${FILE}"
done
[[ "$(grep -c '^Deleted:' "${TEST_ROOT}/success.log")" == 11 ]] || fail 'deleted-target report is incomplete'
for ENTRY in bin/mailcli bin dist/package dist/nested/'file with spaces' dist/nested dist \
  graphify-out/graph.json graphify-out .coverage/report .coverage coverage.out coverage-unit.out \
  mailstore.test cpu.prof heap.pprof runtime.trace 'space profile.prof'; do
  grep -Fq "${FIXTURE}/${ENTRY}" "${TEST_ROOT}/success.log" || fail "removed entry was not reported: ${ENTRY}"
done
for LABEL in 'Repository before:' 'Repository after:' 'Freed in repository:'; do
  case "${LABEL}" in
    'Repository before:') EXPECTED="${BEFORE}" ;;
    'Repository after:') EXPECTED="${AFTER}" ;;
    *) EXPECTED="$((BEFORE - AFTER))" ;;
  esac
  REPORTED="$(reported_kib "${LABEL}" "${TEST_ROOT}/success.log")"
  awk -v actual="${REPORTED}" -v expected="${EXPECTED}" 'BEGIN {
    difference = actual - expected; if (difference < 0) difference = -difference
    exit difference > 5.12
  }' || fail "incorrect allocation report: ${LABEL}"
done
grep -Eq '^Deleted: bin/ \([1-9][0-9.]* MiB\)$' "${TEST_ROOT}/success.log" || fail 'binary allocation is missing'
cat "${TEST_ROOT}/success.log"
"${FIXTURE}/cleanup.sh" >"${TEST_ROOT}/repeat.log"
grep -Fxq 'Freed in repository: 0 KiB' "${TEST_ROOT}/repeat.log" || fail 'repeated cleanup did not reclaim zero'
if grep -q '^Deleted:' "${TEST_ROOT}/repeat.log"; then fail 'repeated cleanup claimed a deletion'; fi

for CASE in tracked-file tracked-child target-link nested-link wrong-type wrong-directory active-lease linked-script nested-script; do
  new_repository "${CASE}"
  mkdir "${FIXTURE}/bin"
  printf 'preserved sentinel\n' >"${FIXTURE}/bin/sentinel"
  case "${CASE}" in
    tracked-file | tracked-child)
      FILE='coverage[kept].out'
      [[ "${CASE}" != tracked-child ]] || FILE='bin/kept[child]'
      printf 'indexed content\n' >"${FIXTURE}/${FILE}"
      BLOB="$(git -C "${FIXTURE}" hash-object -w "${FILE}")"
      git -C "${FIXTURE}" update-index --add --cacheinfo "100644,${BLOB},${FILE}"
      expect_refusal 'tracked content:'
      [[ -f "${FIXTURE}/${FILE}" ]] || fail 'indexed content was deleted'
      ;;
    target-link | nested-link)
      LINK="${FIXTURE}/dist"
      [[ "${CASE}" != nested-link ]] || { mkdir "${LINK}"; LINK="${LINK}/outside"; }
      ln -s "${CALLER}" "${LINK}"
      expect_refusal 'symbolic link:'
      [[ -L "${LINK}" && -f "${CALLER}/bin/sentinel" ]] || fail 'symlink or external target changed'
      ;;
    wrong-type)
      mkdir "${FIXTURE}/wrong.test"
      expect_refusal 'expected regular file:'
      ;;
    wrong-directory)
      printf 'unexpected file\n' >"${FIXTURE}/dist"
      expect_refusal 'expected directory:'
      ;;
    active-lease)
      mkdir "${FIXTURE}/.git/mailcli-write-lease"
      expect_refusal 'a writer lease is active'
      ;;
    linked-script)
      ln -s "${FIXTURE}/cleanup.sh" "${FIXTURE}/linked.sh"
      expect_refusal 'the script must not be a symbolic link' "${FIXTURE}/linked.sh"
      ;;
    nested-script)
      mkdir "${FIXTURE}/nested"
      cp "${PRODUCT_ROOT}/cleanup.sh" "${FIXTURE}/nested/cleanup.sh"
      expect_refusal 'the script must be at the checkout root' "${FIXTURE}/nested/cleanup.sh"
      ;;
  esac
done

new_repository 'CaseCheckout'
CASE_ALIAS="${TEST_ROOT}/casecheckout"
# Alternate spelling denotes this directory only on a case-insensitive filesystem.
if [[ "${CASE_ALIAS}" -ef "${FIXTURE}" ]]; then
  mkdir "${FIXTURE}/bin"
  printf 'generated binary\n' >"${FIXTURE}/bin/mailcli"
  printf 'retained state\n' >"${FIXTURE}/retained.txt"
  CASE_PROTECTED_BEFORE="$(shasum -a 256 "${FIXTURE}/cleanup.sh" "${FIXTURE}/retained.txt" "${FIXTURE}/.git/config")"
  (cd "${CASE_ALIAS}" && ./cleanup.sh) >"${TEST_ROOT}/case-alias.log"
  [[ ! -e "${FIXTURE}/bin" ]] || fail 'alternate-case root invocation did not clean its own checkout'
  [[ "$(shasum -a 256 "${FIXTURE}/cleanup.sh" "${FIXTURE}/retained.txt" "${FIXTURE}/.git/config")" == "${CASE_PROTECTED_BEFORE}" ]] ||
    fail 'alternate-case root invocation changed protected state'
  grep -Fq '/casecheckout/bin/mailcli' "${TEST_ROOT}/case-alias.log" || fail 'alternate-case deletion was not reported'
  printf 'alternate_case_cleanup=passed\n'
fi

new_repository 'empty output directories'
mkdir "${FIXTURE}/bin" "${FIXTURE}/dist" "${FIXTURE}/graphify-out" "${FIXTURE}/.coverage"
"${FIXTURE}/cleanup.sh" >"${TEST_ROOT}/empty.log"
[[ "$(grep -c '^Deleted:' "${TEST_ROOT}/empty.log")" == 4 ]] || fail 'empty directories were not reported'
for DIRECTORY in bin dist graphify-out .coverage; do
  [[ ! -e "${FIXTURE}/${DIRECTORY}" ]] || fail 'an empty output directory remains'
done

new_repository 'arguments refused'
mkdir "${FIXTURE}/bin"
printf 'preserved sentinel\n' >"${FIXTURE}/bin/sentinel"
STATUS=0
"${FIXTURE}/cleanup.sh" --all >"${TEST_ROOT}/arguments.log" 2>&1 || STATUS=$?
[[ "${STATUS}" == 2 && -f "${FIXTURE}/bin/sentinel" ]] || fail 'unexpected arguments did not refuse cleanup'

FIXTURE="${TEST_ROOT}/not a checkout"
mkdir -p "${FIXTURE}/bin"
cp "${PRODUCT_ROOT}/cleanup.sh" "${FIXTURE}/cleanup.sh"
printf 'preserved sentinel\n' >"${FIXTURE}/bin/sentinel"
expect_refusal 'a Git checkout is required'

new_repository 'deletion failure'
mkdir "${FIXTURE}/bin" "${FIXTURE}/dist"
printf 'partial removal\n' >"${FIXTURE}/bin/sentinel"
printf 'not reached\n' >"${FIXTURE}/dist/preserved"
chmod a-w "${FIXTURE}"
STATUS=0
"${FIXTURE}/cleanup.sh" >"${TEST_ROOT}/failure.log" 2>&1 || STATUS=$?
chmod u+w "${FIXTURE}"
[[ "${STATUS}" == 1 ]] || fail 'actual permission failure was not propagated'
grep -Fq 'Cleanup failed: bin/' "${TEST_ROOT}/failure.log" || fail 'failed deletion is missing'
grep -q '^Repository after:' "${TEST_ROOT}/failure.log" || fail 'partial cleanup omitted final size'
[[ ! -e "${FIXTURE}/bin/sentinel" ]] || fail 'the permission fixture did not partially remove its target'
grep -Fq "${FIXTURE}/bin/sentinel" "${TEST_ROOT}/failure.log" || fail 'partial successful removal was not reported'
[[ -d "${FIXTURE}/bin" && -f "${FIXTURE}/dist/preserved" ]] || fail 'cleanup continued after deletion failure'
if grep -q '^Deleted:' "${TEST_ROOT}/failure.log"; then fail 'partial removal was reported as complete'; fi
printf 'cleanup_test=passed\n'

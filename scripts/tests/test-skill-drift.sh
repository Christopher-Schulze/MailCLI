#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
DRIFT_REPORT="${MAILCLI_ROOT}/scripts/tests/report-skill-drift.sh"
REPOSITORY_SKILL="${MAILCLI_ROOT}/skills/mailcli"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-skill-drift-test.XXXXXX")"
PACKAGE_ROOT="${TEST_ROOT}/package"
PACKAGE_SKILL="${PACKAGE_ROOT}/skills/mailcli"
GLOBAL_SKILL="${TEST_ROOT}/global/.agents/skills/mailcli"
ISOLATED_HOME="${TEST_ROOT}/isolated-home"

cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-skill-drift-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

[[ -x "${DRIFT_REPORT}" ]] || {
  printf 'Skill drift report is missing or not executable: %s\n' "${DRIFT_REPORT}" >&2
  exit 1
}
[[ -d "${REPOSITORY_SKILL}" ]] || {
  printf 'Repository skill is missing: %s\n' "${REPOSITORY_SKILL}" >&2
  exit 1
}
if [[ ! -x "${MAILCLI_ROOT}/bin/mailcli" ]]; then
  "${MAILCLI_ROOT}/scripts/build/build.sh" >/dev/null
fi

mkdir -p "${PACKAGE_ROOT}/bin" "${PACKAGE_ROOT}/skills" "${GLOBAL_SKILL%/*}"
cp "${MAILCLI_ROOT}/bin/mailcli" "${PACKAGE_ROOT}/bin/mailcli"
cp -R "${REPOSITORY_SKILL}" "${PACKAGE_SKILL}"
cp "${MAILCLI_ROOT}/scripts/release/install.sh" "${PACKAGE_ROOT}/install.sh"
chmod 0755 "${PACKAGE_ROOT}/bin/mailcli" "${PACKAGE_ROOT}/install.sh"
cp -R "${PACKAGE_SKILL}" "${GLOBAL_SKILL}"

REPORT_OUTPUT="$("${DRIFT_REPORT}" --repository "${REPOSITORY_SKILL}" --installed "${PACKAGE_SKILL}")"
grep -Fqx 'status=match' <<<"${REPORT_OUTPUT}"

GLOBAL_BEFORE_PACKAGE_MISMATCH="$(shasum -a 256 "${GLOBAL_SKILL}/SKILL.md" | awk '{print $1}')"
printf '\npackage drift\n' >>"${PACKAGE_SKILL}/SKILL.md"
if "${DRIFT_REPORT}" --repository "${REPOSITORY_SKILL}" --installed "${PACKAGE_SKILL}" >"${TEST_ROOT}/package-mismatch.out" 2>&1; then
  printf 'Isolated consistency check accepted a mismatched package skill\n' >&2
  exit 1
fi
grep -Fqx 'status=mismatch' "${TEST_ROOT}/package-mismatch.out"
GLOBAL_AFTER_PACKAGE_MISMATCH="$(shasum -a 256 "${GLOBAL_SKILL}/SKILL.md" | awk '{print $1}')"
[[ "${GLOBAL_AFTER_PACKAGE_MISMATCH}" == "${GLOBAL_BEFORE_PACKAGE_MISMATCH}" ]]
rm -rf "${PACKAGE_SKILL}"
cp -R "${REPOSITORY_SKILL}" "${PACKAGE_SKILL}"

if MAILCLI_SKILL_DESTINATION= HOME="${TEST_ROOT}/missing-home" \
  "${DRIFT_REPORT}" --repository "${REPOSITORY_SKILL}" >"${TEST_ROOT}/missing.out" 2>&1; then
  printf 'Drift check accepted a missing default installation\n' >&2
  exit 1
fi
grep -Fqx 'status=missing' "${TEST_ROOT}/missing.out"
[[ ! -e "${TEST_ROOT}/missing-home/.agents/skills/mailcli" ]]

mkdir -p "${TEST_ROOT}/stale/.agents/skills/mailcli"
printf 'stale skill\n' >"${TEST_ROOT}/stale/.agents/skills/mailcli/SKILL.md"
printf 'stale agent\n' >"${TEST_ROOT}/stale/.agents/skills/mailcli/agents.yaml"
STALE_BEFORE="$(shasum -a 256 "${TEST_ROOT}/stale/.agents/skills/mailcli/SKILL.md" | awk '{print $1}')"
if "${DRIFT_REPORT}" --repository "${REPOSITORY_SKILL}" \
  --installed "${TEST_ROOT}/stale/.agents/skills/mailcli" >"${TEST_ROOT}/stale.out" 2>&1; then
  printf 'Drift check accepted a stale installation\n' >&2
  exit 1
fi
grep -Fqx 'status=mismatch' "${TEST_ROOT}/stale.out"
STALE_AFTER="$(shasum -a 256 "${TEST_ROOT}/stale/.agents/skills/mailcli/SKILL.md" | awk '{print $1}')"
[[ "${STALE_AFTER}" == "${STALE_BEFORE}" ]]

mkdir -p "${ISOLATED_HOME}"
MAILCLI_BINARY_DESTINATION="${ISOLATED_HOME}/.local/bin/mailcli" \
MAILCLI_SKILL_DESTINATION="${ISOLATED_HOME}/.agents/skills/mailcli" \
MAILCLI_INSTALL_PACKAGE_ROOT="${PACKAGE_ROOT}" \
  HOME="${ISOLATED_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1 || {
  printf 'Isolated package installation failed\n' >&2
  exit 1
}
REPORT_OUTPUT="$("${DRIFT_REPORT}" --repository "${REPOSITORY_SKILL}" \
  --installed "${ISOLATED_HOME}/.agents/skills/mailcli")"
grep -Fqx 'status=match' <<<"${REPORT_OUTPUT}"

MUTATE_ENV="${TEST_ROOT}/mutate-after-copy.sh"
MUTATE_COUNT="${TEST_ROOT}/mutate-count"
cat >"${MUTATE_ENV}" <<'EOF'
cp() {
  command cp "$@"
  local count=0
  if [[ -f "${MAILCLI_DRIFT_TEST_COUNT}" ]]; then
    count="$(<"${MAILCLI_DRIFT_TEST_COUNT}")"
  fi
  count=$((count + 1))
  printf '%s\n' "${count}" >"${MAILCLI_DRIFT_TEST_COUNT}"
  if [[ "${count}" -eq 2 ]]; then
    printf '\nchanged during check\n' >>"${MAILCLI_DRIFT_TEST_TARGET}/SKILL.md"
  fi
}
EOF
cp -R "${REPOSITORY_SKILL}" "${GLOBAL_SKILL}"
rm -f "${MUTATE_COUNT}"
set +e
MAILCLI_DRIFT_TEST_COUNT="${MUTATE_COUNT}" \
MAILCLI_DRIFT_TEST_TARGET="${GLOBAL_SKILL}" \
  BASH_ENV="${MUTATE_ENV}" "${DRIFT_REPORT}" \
  --repository "${REPOSITORY_SKILL}" --installed "${GLOBAL_SKILL}" >"${TEST_ROOT}/unstable.out" 2>&1
UNSTABLE_STATUS=$?
set -e
[[ "${UNSTABLE_STATUS}" -eq 1 ]]
grep -Fqx 'status=unstable' "${TEST_ROOT}/unstable.out"

printf 'Skill validation and drift tests passed\n'

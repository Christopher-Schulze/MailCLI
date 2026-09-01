#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-release-test.XXXXXX")"
cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-release-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

RELEASE_DIRECTORY="${TEST_ROOT}/release"
if MAILCLI_RELEASE_DIRECTORY="${TEST_ROOT}/version-mismatch" \
  "${MAILCLI_ROOT}/scripts/release/build-release.sh" 0.1.0 >/dev/null 2>&1; then
  printf 'Release builder accepted a version that disagrees with the binary\n' >&2
  exit 1
fi
MAILCLI_RELEASE_DIRECTORY="${RELEASE_DIRECTORY}" \
  "${MAILCLI_ROOT}/scripts/release/build-release.sh" 1.0.3

ARCHIVE="${RELEASE_DIRECTORY}/mailcli_1.0.3_darwin_arm64.tar.gz"
CHECKSUMS="${RELEASE_DIRECTORY}/SHA256SUMS"
[[ -f "${ARCHIVE}" && -f "${CHECKSUMS}" ]]
(
  cd "${RELEASE_DIRECTORY}"
  shasum -a 256 -c SHA256SUMS
)

ARCHIVE_LIST="${TEST_ROOT}/archive-list.txt"
tar -tzf "${ARCHIVE}" >"${ARCHIVE_LIST}"
if grep -Eq '(^|/)(\.DS_Store|\._[^/]*)$' "${ARCHIVE_LIST}"; then
  printf 'Release archive contains macOS metadata files\n' >&2
  exit 1
fi
for REQUIRED_PATH in \
  mailcli_1.0.3_darwin_arm64/bin/mailcli \
  mailcli_1.0.3_darwin_arm64/skills/mailcli/SKILL.md \
  mailcli_1.0.3_darwin_arm64/skills/mailcli/agents/openai.yaml \
  mailcli_1.0.3_darwin_arm64/install.sh \
  mailcli_1.0.3_darwin_arm64/README.md \
  mailcli_1.0.3_darwin_arm64/LICENSE; do
  if ! grep -Fxq "${REQUIRED_PATH}" "${ARCHIVE_LIST}"; then
    printf 'Release archive is missing %s\n' "${REQUIRED_PATH}" >&2
    exit 1
  fi
done

tar -xzf "${ARCHIVE}" -C "${TEST_ROOT}"
PACKAGE_ROOT="${TEST_ROOT}/mailcli_1.0.3_darwin_arm64"
"${MAILCLI_ROOT}/scripts/build/build.sh" >/dev/null
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${MAILCLI_ROOT}/bin/mailcli"
TEST_HOME="${TEST_ROOT}/home"
mkdir -p "${TEST_HOME}"
HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh"

INSTALLED_BINARY="${TEST_HOME}/.local/bin/mailcli"
INSTALLED_SKILL="${TEST_HOME}/.agents/skills/mailcli"
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${INSTALLED_BINARY}"
diff -qr "${PACKAGE_ROOT}/skills/mailcli" "${INSTALLED_SKILL}" >/dev/null
[[ "$("${INSTALLED_BINARY}" version)" == "mailcli 1.0.3" ]]
"${INSTALLED_BINARY}" capabilities --json | grep -q '"raw_mime_send":false'
file "${INSTALLED_BINARY}" | grep -q 'Mach-O 64-bit executable arm64'

HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh"
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${INSTALLED_BINARY}"
diff -qr "${PACKAGE_ROOT}/skills/mailcli" "${INSTALLED_SKILL}" >/dev/null

printf 'old binary\n' >"${INSTALLED_BINARY}"
chmod 0755 "${INSTALLED_BINARY}"
printf 'old skill\n' >"${INSTALLED_SKILL}/SKILL.md"
HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh"
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${INSTALLED_BINARY}"
diff -qr "${PACKAGE_ROOT}/skills/mailcli" "${INSTALLED_SKILL}" >/dev/null
[[ ! -e "${INSTALLED_BINARY}.mailcli-backup" ]]
[[ ! -e "${INSTALLED_SKILL}.mailcli-backup" ]]

cp "${INSTALLED_BINARY}" "${TEST_ROOT}/binary-before-refusal"
cp -R "${INSTALLED_SKILL}" "${TEST_ROOT}/skill-before-refusal"
mkdir "${INSTALLED_SKILL}.mailcli-backup"
if HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer unexpectedly replaced content while a backup path existed\n' >&2
  exit 1
fi
cmp -s "${TEST_ROOT}/binary-before-refusal" "${INSTALLED_BINARY}"
diff -qr "${TEST_ROOT}/skill-before-refusal" "${INSTALLED_SKILL}" >/dev/null
rmdir "${INSTALLED_SKILL}.mailcli-backup"

printf 'rollback binary\n' >"${INSTALLED_BINARY}"
chmod 0755 "${INSTALLED_BINARY}"
printf 'rollback skill\n' >"${INSTALLED_SKILL}/SKILL.md"
cp "${INSTALLED_BINARY}" "${TEST_ROOT}/binary-before-rollback"
cp -R "${INSTALLED_SKILL}" "${TEST_ROOT}/skill-before-rollback"
MV_FAILURE_ENV="${TEST_ROOT}/fail-fourth-move.sh"
# The generated BASH_ENV must expand inside the child shell.
# shellcheck disable=SC2016
printf '%s\n' \
  'MAILCLI_TEST_MV_COUNT=0' \
  'mv() {' \
  '  MAILCLI_TEST_MV_COUNT=$((MAILCLI_TEST_MV_COUNT + 1))' \
  '  if [[ "${MAILCLI_TEST_MV_COUNT}" -eq 4 ]]; then' \
  '    return 71' \
  '  fi' \
  '  command mv "$@"' \
  '}' >"${MV_FAILURE_ENV}"
if BASH_ENV="${MV_FAILURE_ENV}" HOME="${TEST_HOME}" \
  "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer unexpectedly succeeded after an injected staged-move failure\n' >&2
  exit 1
fi
cmp -s "${TEST_ROOT}/binary-before-rollback" "${INSTALLED_BINARY}"
diff -qr "${TEST_ROOT}/skill-before-rollback" "${INSTALLED_SKILL}" >/dev/null
[[ ! -e "${INSTALLED_BINARY}.mailcli-backup" ]]
[[ ! -e "${INSTALLED_SKILL}.mailcli-backup" ]]
HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null

if grep -Eq 'xattr|spctl[[:space:]]+--master-disable' "${PACKAGE_ROOT}/install.sh"; then
  printf 'Installer must not bypass macOS security controls\n' >&2
  exit 1
fi
if MAILCLI_BINARY_DESTINATION="${TEST_ROOT}/overlap" \
  MAILCLI_SKILL_DESTINATION="${TEST_ROOT}/overlap/skill" \
  HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer accepted overlapping destinations\n' >&2
  exit 1
fi
if MAILCLI_BINARY_DESTINATION="${TEST_ROOT}/backup-overlap/mailcli" \
  MAILCLI_SKILL_DESTINATION="${TEST_ROOT}/backup-overlap/mailcli.mailcli-backup/skill" \
  HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer accepted a destination nested under another destination backup\n' >&2
  exit 1
fi

printf 'Release packaging and installation tests passed\n'

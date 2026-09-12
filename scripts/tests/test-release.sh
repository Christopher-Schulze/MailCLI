#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Release verification requires command: %s\n' "$1" >&2
    exit 2
  fi
}

for command_name in go shasum tar file size codesign diff grep wc; do
  require_command "${command_name}"
done
if [[ "$(uname -s)" != "Darwin" || "$(uname -m)" != "arm64" ]]; then
  printf 'Release verification requires macOS on darwin/arm64; found %s/%s\n' \
    "$(uname -s)" "$(uname -m)" >&2
  exit 2
fi
GO_VERSION="$(go env GOVERSION)"
if [[ ! "${GO_VERSION}" =~ ^go1\.([0-9]+)\. ]]; then
  printf 'Release verification could not parse Go version: %s\n' "${GO_VERSION}" >&2
  exit 2
fi
GO_MINOR="${BASH_REMATCH[1]}"
if ((GO_MINOR < 27)); then
  printf 'Release verification requires Go 1.27 or newer: %s\n' "${GO_VERSION}" >&2
  exit 2
fi
if ! (cd "${MAILCLI_ROOT}" && go mod verify); then
  printf 'Go module verification failed before release work began\n' >&2
  exit 1
fi
if ! (cd "${MAILCLI_ROOT}" && go build -mod=readonly ./...); then
  printf 'Source build failed before native release packaging\n' >&2
  exit 1
fi

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-release-test.XXXXXX")"
cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-release-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

RELEASE_DIRECTORY="${TEST_ROOT}/release"
TEST_SIGNING_KEY="${TEST_ROOT}/release-signing-key"
TEST_PUBLIC_KEY="$(go run -mod=readonly "${MAILCLI_ROOT}/cmd/mailcli-release-sign" keygen --private "${TEST_SIGNING_KEY}")"
if "${MAILCLI_ROOT}/scripts/release/build-release.sh" >/dev/null 2>&1; then
  printf 'Release builder accepted a missing version argument\n' >&2
  exit 1
fi
"${MAILCLI_ROOT}/scripts/build/build.sh" >/dev/null
TEST_VERSION="$("${MAILCLI_ROOT}/bin/mailcli" version)"
TEST_VERSION="${TEST_VERSION#mailcli }"
if [[ ! "${TEST_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Built binary reports a malformed version: %s\n' "${TEST_VERSION}" >&2
  exit 1
fi
if MAILCLI_RELEASE_DIRECTORY="${TEST_ROOT}/version-mismatch" \
  "${MAILCLI_ROOT}/scripts/release/build-release.sh" 0.1.0 >/dev/null 2>&1; then
  printf 'Release builder accepted a version that disagrees with the binary\n' >&2
  exit 1
fi
MAILCLI_RELEASE_DIRECTORY="${RELEASE_DIRECTORY}" \
  MAILCLI_RELEASE_SIGNING_KEY="${TEST_SIGNING_KEY}" \
  MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY="${TEST_PUBLIC_KEY}" \
  "${MAILCLI_ROOT}/scripts/release/build-release.sh" "${TEST_VERSION}"

ARCHIVE="${RELEASE_DIRECTORY}/mailcli_${TEST_VERSION}_darwin_arm64.tar.gz"
CHECKSUMS="${RELEASE_DIRECTORY}/SHA256SUMS"
SIGNATURE="${RELEASE_DIRECTORY}/SHA256SUMS.sig"
[[ -f "${ARCHIVE}" && -f "${CHECKSUMS}" && -f "${SIGNATURE}" ]]
(
  cd "${RELEASE_DIRECTORY}"
  shasum -a 256 -c SHA256SUMS
)
go run -mod=readonly "${MAILCLI_ROOT}/cmd/mailcli-release-sign" verify \
  --public "${TEST_PUBLIC_KEY}" \
  --input "${CHECKSUMS}" \
  --signature "${SIGNATURE}"

ARCHIVE_LIST="${TEST_ROOT}/archive-list.txt"
tar -tzf "${ARCHIVE}" >"${ARCHIVE_LIST}"
if grep -Eq '(^|/)(\.DS_Store|\._[^/]*)$' "${ARCHIVE_LIST}"; then
  printf 'Release archive contains macOS metadata files\n' >&2
  exit 1
fi
for REQUIRED_PATH in \
  mailcli_${TEST_VERSION}_darwin_arm64/bin/mailcli \
  mailcli_${TEST_VERSION}_darwin_arm64/skills/mailcli/SKILL.md \
  mailcli_${TEST_VERSION}_darwin_arm64/skills/mailcli/agents/openai.yaml \
  mailcli_${TEST_VERSION}_darwin_arm64/install.sh \
  mailcli_${TEST_VERSION}_darwin_arm64/README.md \
  mailcli_${TEST_VERSION}_darwin_arm64/LICENSE; do
  if ! grep -Fxq "${REQUIRED_PATH}" "${ARCHIVE_LIST}"; then
    printf 'Release archive is missing %s\n' "${REQUIRED_PATH}" >&2
    exit 1
  fi
done

tar -xzf "${ARCHIVE}" -C "${TEST_ROOT}"
PACKAGE_ROOT="${TEST_ROOT}/mailcli_${TEST_VERSION}_darwin_arm64"
SOURCE_BINARY_COPY="${TEST_ROOT}/release-source-binary"
cp "${PACKAGE_ROOT}/bin/mailcli" "${SOURCE_BINARY_COPY}"
"${MAILCLI_ROOT}/scripts/build/build.sh" >/dev/null
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${MAILCLI_ROOT}/bin/mailcli"
TEST_HOME="${TEST_ROOT}/home"
mkdir -p "${TEST_HOME}"
HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh"

INSTALLED_BINARY="${TEST_HOME}/.local/bin/mailcli"
INSTALLED_SKILL="${TEST_HOME}/.agents/skills/mailcli"
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${INSTALLED_BINARY}"
diff -qr "${PACKAGE_ROOT}/skills/mailcli" "${INSTALLED_SKILL}" >/dev/null
(
  cd "${MAILCLI_ROOT}"
  MAILCLI_TEST_SKILL_DIRECTORY="${INSTALLED_SKILL}" \
    go test ./internal/cli -run '^TestSkillDocumentationSelfContained$' -count=1
)
[[ "$("${INSTALLED_BINARY}" version)" == "mailcli ${TEST_VERSION}" ]]
CAPABILITIES_JSON="$("${INSTALLED_BINARY}" capabilities --json)"
if ! grep -Fq '"raw_mime_send":true' <<<"${CAPABILITIES_JSON}"; then
  printf 'Release binary does not advertise raw_mime_send=true\n' >&2
  exit 1
fi
FILE_DESCRIPTION="$(file "${INSTALLED_BINARY}")"
if [[ "${FILE_DESCRIPTION}" != *'Mach-O 64-bit executable arm64'* ]]; then
  printf 'Installed release binary is not native darwin/arm64: %s\n' "${FILE_DESCRIPTION}" >&2
  exit 1
fi
DWARF_DESCRIPTION="$(size -m "${INSTALLED_BINARY}")"
if grep -Fq 'Segment __DWARF:' <<<"${DWARF_DESCRIPTION}"; then
  printf 'Release binary contains DWARF debug sections\n' >&2
  exit 1
fi
BINARY_BYTES="$(wc -c <"${INSTALLED_BINARY}")"
BINARY_BYTES="${BINARY_BYTES//[[:space:]]/}"
if ((BINARY_BYTES > 12 * 1024 * 1024)); then
  printf 'Release binary exceeds the 12 MiB size budget: %s bytes\n' "${BINARY_BYTES}" >&2
  exit 1
fi

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
cp "${INSTALLED_BINARY}" "${TEST_ROOT}/binary-before-binary-interruption"
cp -R "${INSTALLED_SKILL}" "${TEST_ROOT}/skill-before-binary-interruption"
INTERRUPT_ENV="${TEST_ROOT}/kill-after-live-rename.sh"
# shellcheck disable=SC2016
printf '%s\n' \
  'mv() {' \
  '  if [[ "${2:-}" == "${MAILCLI_TEST_INTERRUPT_PATH:-}" ]]; then' \
  '    command mv "$@"' \
  '    kill -KILL "$$"' \
  '  fi' \
  '  command mv "$@"' \
  '}' >"${INTERRUPT_ENV}"
set +e
MAILCLI_TEST_INTERRUPT_PATH="${INSTALLED_BINARY}" BASH_ENV="${INTERRUPT_ENV}" HOME="${TEST_HOME}" \
  "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1
INTERRUPTION_STATUS=$?
set -e
[[ "${INTERRUPTION_STATUS}" -eq 137 ]]
printf 'invalid source\n' >"${PACKAGE_ROOT}/bin/mailcli"
chmod 0755 "${PACKAGE_ROOT}/bin/mailcli"
if HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer unexpectedly accepted an invalid source after binary interruption\n' >&2
  exit 1
fi
cmp -s "${TEST_ROOT}/binary-before-binary-interruption" "${INSTALLED_BINARY}"
diff -qr "${TEST_ROOT}/skill-before-binary-interruption" "${INSTALLED_SKILL}" >/dev/null
cp "${SOURCE_BINARY_COPY}" "${PACKAGE_ROOT}/bin/mailcli"
chmod 0755 "${PACKAGE_ROOT}/bin/mailcli"
HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null

printf 'rollback binary again\n' >"${INSTALLED_BINARY}"
chmod 0755 "${INSTALLED_BINARY}"
printf 'rollback skill again\n' >"${INSTALLED_SKILL}/SKILL.md"
cp "${INSTALLED_BINARY}" "${TEST_ROOT}/binary-before-skill-interruption"
cp -R "${INSTALLED_SKILL}" "${TEST_ROOT}/skill-before-skill-interruption"
set +e
MAILCLI_TEST_INTERRUPT_PATH="${INSTALLED_SKILL}" BASH_ENV="${INTERRUPT_ENV}" HOME="${TEST_HOME}" \
  "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1
INTERRUPTION_STATUS=$?
set -e
[[ "${INTERRUPTION_STATUS}" -eq 137 ]]
printf 'invalid source\n' >"${PACKAGE_ROOT}/bin/mailcli"
chmod 0755 "${PACKAGE_ROOT}/bin/mailcli"
if HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer unexpectedly accepted an invalid source after skill interruption\n' >&2
  exit 1
fi
cmp -s "${TEST_ROOT}/binary-before-skill-interruption" "${INSTALLED_BINARY}"
diff -qr "${TEST_ROOT}/skill-before-skill-interruption" "${INSTALLED_SKILL}" >/dev/null
cp "${SOURCE_BINARY_COPY}" "${PACKAGE_ROOT}/bin/mailcli"
chmod 0755 "${PACKAGE_ROOT}/bin/mailcli"
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

SYMLINK_ROOT="${TEST_ROOT}/symlink-check"
mkdir -p "${SYMLINK_ROOT}/outside" "${SYMLINK_ROOT}/safe"
ln -s "${SYMLINK_ROOT}/outside" "${SYMLINK_ROOT}/linked-parent"
if MAILCLI_BINARY_DESTINATION="${SYMLINK_ROOT}/linked-parent/mailcli" \
  MAILCLI_SKILL_DESTINATION="${SYMLINK_ROOT}/safe/skill" \
  HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer accepted a symbolic-link parent directory\n' >&2
  exit 1
fi
[[ ! -e "${SYMLINK_ROOT}/outside/mailcli" ]]
ln -s "${SYMLINK_ROOT}/outside/missing" "${SYMLINK_ROOT}/linked-destination"
if MAILCLI_BINARY_DESTINATION="${SYMLINK_ROOT}/linked-destination" \
  MAILCLI_SKILL_DESTINATION="${SYMLINK_ROOT}/safe/skill" \
  HOME="${TEST_HOME}" "${PACKAGE_ROOT}/install.sh" >/dev/null 2>&1; then
  printf 'Installer accepted a symbolic-link destination\n' >&2
  exit 1
fi

printf 'Release packaging and installation tests passed\n'

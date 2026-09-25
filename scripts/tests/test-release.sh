#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Release verification requires command: %s\n' "$1" >&2
    exit 2
  fi
}

for command_name in go shasum tar file size codesign diff grep wc awk link stat env; do
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
BUILD_OUTPUT="${MAILCLI_BUILD_OUTPUT:-${TEST_ROOT}/build/mailcli}"
export MAILCLI_BUILD_OUTPUT="${BUILD_OUTPUT}"
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
TEST_VERSION="$("${BUILD_OUTPUT}" version)"
TEST_VERSION="${TEST_VERSION#mailcli }"
if [[ ! "${TEST_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Built binary reports a malformed version: %s\n' "${TEST_VERSION}" >&2
  exit 1
fi
assert_no_release_assets() {
  local RELEASE_DIRECTORY="$1"
  local VERSION="$2"
  local ASSET_NAME
  for ASSET_NAME in "mailcli_${VERSION}_darwin_arm64.tar.gz" SHA256SUMS SHA256SUMS.sig; do
    if [[ -e "${RELEASE_DIRECTORY}/${ASSET_NAME}" || -L "${RELEASE_DIRECTORY}/${ASSET_NAME}" ]]; then
      printf 'Unexpected release asset after failed build: %s\n' "${RELEASE_DIRECTORY}/${ASSET_NAME}" >&2
      return 1
    fi
  done
  if [[ -e "${RELEASE_DIRECTORY}/.mailcli-release-staging-${VERSION}" ||
    -L "${RELEASE_DIRECTORY}/.mailcli-release-staging-${VERSION}" ]]; then
    printf 'Unexpected release staging directory after failed build: %s\n' \
      "${RELEASE_DIRECTORY}/.mailcli-release-staging-${VERSION}" >&2
    return 1
  fi
}
run_test_release_builder() {
  local RELEASE_DIRECTORY="$1"
  shift
  env \
    MAILCLI_RELEASE_DIRECTORY="${RELEASE_DIRECTORY}" \
    MAILCLI_RELEASE_SIGNING_KEY="${TEST_SIGNING_KEY}" \
    MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY="${TEST_PUBLIC_KEY}" \
    "$@" \
    "${MAILCLI_ROOT}/scripts/release/build-release.sh" "${TEST_VERSION}"
}
VERSION_MISMATCH_DIRECTORY="${TEST_ROOT}/version-mismatch"
if MAILCLI_RELEASE_DIRECTORY="${VERSION_MISMATCH_DIRECTORY}" \
  "${MAILCLI_ROOT}/scripts/release/build-release.sh" 0.1.0 >/dev/null 2>&1; then
  printf 'Release builder accepted a version that disagrees with the binary\n' >&2
  exit 1
fi
assert_no_release_assets "${VERSION_MISMATCH_DIRECTORY}" "0.1.0"
run_test_release_builder "${RELEASE_DIRECTORY}"

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
if [[ -e "${RELEASE_DIRECTORY}/.mailcli-release-staging-${TEST_VERSION}" ||
  -L "${RELEASE_DIRECTORY}/.mailcli-release-staging-${TEST_VERSION}" ]]; then
  printf 'Release builder retained staging after complete publication\n' >&2
  exit 1
fi

TAR_BINARY="$(command -v tar)"
STAGING_FAILURE_DIRECTORY="${TEST_ROOT}/staging-failure"
STAGING_FAILURE_BIN="${TEST_ROOT}/staging-failure-bin"
mkdir -p "${STAGING_FAILURE_BIN}"
cat >"${STAGING_FAILURE_BIN}/tar" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "${1:-}" == "-czf" ]]; then
  printf 'injected staging ENOSPC\n' >&2
  exit 28
fi
exec "${MAILCLI_TEST_REAL_TAR}" "$@"
EOF
chmod 0755 "${STAGING_FAILURE_BIN}/tar"
STAGING_FAILURE_LOG="${TEST_ROOT}/staging-failure.log"
if run_test_release_builder "${STAGING_FAILURE_DIRECTORY}" \
  "PATH=${STAGING_FAILURE_BIN}:${PATH}" \
  "MAILCLI_TEST_REAL_TAR=${TAR_BINARY}" >"${STAGING_FAILURE_LOG}" 2>&1; then
  printf 'Release builder ignored a staging ENOSPC failure\n' >&2
  exit 1
fi
grep -Fq 'injected staging ENOSPC' "${STAGING_FAILURE_LOG}"
grep -Fq 'no new final assets were published' "${STAGING_FAILURE_LOG}"
assert_no_release_assets "${STAGING_FAILURE_DIRECTORY}" "${TEST_VERSION}"

MISSING_SIGNING_KEY="${TEST_ROOT}/missing-release-signing-key"
if [[ -e "${MISSING_SIGNING_KEY}" || -L "${MISSING_SIGNING_KEY}" ]]; then
  printf 'Missing-key test path unexpectedly exists\n' >&2
  exit 1
fi
SIGNING_FAILURE_DIRECTORY="${TEST_ROOT}/signing-failure"
SIGNING_FAILURE_LOG="${TEST_ROOT}/signing-failure.log"
if run_test_release_builder "${SIGNING_FAILURE_DIRECTORY}" \
  "MAILCLI_RELEASE_SIGNING_KEY=${MISSING_SIGNING_KEY}" >"${SIGNING_FAILURE_LOG}" 2>&1; then
  printf 'Release builder published assets without a signing key\n' >&2
  exit 1
fi
grep -Fq 'open private key' "${SIGNING_FAILURE_LOG}"
grep -Fq 'no new final assets were published' "${SIGNING_FAILURE_LOG}"
assert_no_release_assets "${SIGNING_FAILURE_DIRECTORY}" "${TEST_VERSION}"

GO_BINARY="$(command -v go)"
VERIFY_FAILURE_DIRECTORY="${TEST_ROOT}/verify-failure"
VERIFY_FAILURE_BIN="${TEST_ROOT}/verify-failure-bin"
mkdir -p "${VERIFY_FAILURE_BIN}"
cat >"${VERIFY_FAILURE_BIN}/go" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "${MAILCLI_TEST_TAMPER_RELEASE_SIGNATURE:-}" == "1" ]]; then
  arguments=("$@")
  for ((argument_index = 0; argument_index < ${#arguments[@]}; argument_index++)); do
    if [[ "${arguments[argument_index]}" == "verify" ]]; then
      for ((signature_index = argument_index + 1; signature_index + 1 < ${#arguments[@]}; signature_index++)); do
        if [[ "${arguments[signature_index]}" == "--signature" ]]; then
          if [[ "${arguments[signature_index + 1]}" == */SHA256SUMS.sig ]]; then
            printf 'invalid injected signature\n' >"${arguments[signature_index + 1]}"
          fi
          break
        fi
      done
      break
    fi
  done
fi
exec "${MAILCLI_TEST_REAL_GO}" "$@"
EOF
chmod 0755 "${VERIFY_FAILURE_BIN}/go"
VERIFY_FAILURE_LOG="${TEST_ROOT}/verify-failure.log"
if run_test_release_builder "${VERIFY_FAILURE_DIRECTORY}" \
  "PATH=${VERIFY_FAILURE_BIN}:${PATH}" \
  'MAILCLI_TEST_TAMPER_RELEASE_SIGNATURE=1' \
  "MAILCLI_TEST_REAL_GO=${GO_BINARY}" >"${VERIFY_FAILURE_LOG}" 2>&1; then
  printf 'Release builder published assets after signature verification failed\n' >&2
  exit 1
fi
grep -Fq 'Release checksum signature verification failed' "${VERIFY_FAILURE_LOG}"
assert_no_release_assets "${VERIFY_FAILURE_DIRECTORY}" "${TEST_VERSION}"

CP_BINARY="$(command -v cp)"
PUBLICATION_FAILURE_DIRECTORY="${TEST_ROOT}/publication-failure"
PUBLICATION_FAILURE_BIN="${TEST_ROOT}/publication-failure-bin"
PUBLICATION_COPY_COUNT="${TEST_ROOT}/publication-copy-count"
mkdir -p "${PUBLICATION_FAILURE_BIN}"
cat >"${PUBLICATION_FAILURE_BIN}/cp" <<'EOF'
#!/bin/bash
set -euo pipefail
destination=''
for argument in "$@"; do
  destination="${argument}"
done
case "${destination}" in
  */.publish.*)
    count=0
    if [[ -f "${MAILCLI_TEST_PUBLICATION_COUNT}" ]]; then
      IFS= read -r count <"${MAILCLI_TEST_PUBLICATION_COUNT}"
    fi
    count=$((count + 1))
    printf '%s\n' "${count}" >"${MAILCLI_TEST_PUBLICATION_COUNT}"
    if [[ "${count}" -eq 2 ]]; then
      printf 'injected publication ENOSPC\n' >&2
      exit 28
    fi
    ;;
esac
exec "${MAILCLI_TEST_REAL_CP}" "$@"
EOF
chmod 0755 "${PUBLICATION_FAILURE_BIN}/cp"
PUBLICATION_FAILURE_LOG="${TEST_ROOT}/publication-failure.log"
if run_test_release_builder "${PUBLICATION_FAILURE_DIRECTORY}" \
  "PATH=${PUBLICATION_FAILURE_BIN}:${PATH}" \
  "MAILCLI_TEST_REAL_CP=${CP_BINARY}" \
  "MAILCLI_TEST_PUBLICATION_COUNT=${PUBLICATION_COPY_COUNT}" \
  >"${PUBLICATION_FAILURE_LOG}" 2>&1; then
  printf 'Release builder ignored a publication ENOSPC failure\n' >&2
  exit 1
fi
PARTIAL_STAGE="${PUBLICATION_FAILURE_DIRECTORY}/.mailcli-release-staging-${TEST_VERSION}"
PARTIAL_ARCHIVE="${PUBLICATION_FAILURE_DIRECTORY}/mailcli_${TEST_VERSION}_darwin_arm64.tar.gz"
PARTIAL_CHECKSUMS="${PUBLICATION_FAILURE_DIRECTORY}/SHA256SUMS"
PARTIAL_SIGNATURE="${PUBLICATION_FAILURE_DIRECTORY}/SHA256SUMS.sig"
[[ -d "${PARTIAL_STAGE}" && ! -L "${PARTIAL_STAGE}" ]]
[[ -f "${PARTIAL_ARCHIVE}" && ! -e "${PARTIAL_CHECKSUMS}" && ! -e "${PARTIAL_SIGNATURE}" ]]
[[ ! "${PARTIAL_ARCHIVE}" -ef "${PARTIAL_STAGE}/mailcli_${TEST_VERSION}_darwin_arm64.tar.gz" ]]
PARTIAL_STAGE_MODE="$(stat -f '%Lp' "${PARTIAL_STAGE}")"
[[ "${PARTIAL_STAGE_MODE}" == "700" || "${PARTIAL_STAGE_MODE}" == "0700" ]]
grep -Fq 'injected publication ENOSPC' "${PUBLICATION_FAILURE_LOG}"
grep -Fqx "matching_final=${PARTIAL_ARCHIVE}" "${PUBLICATION_FAILURE_LOG}"
grep -Fqx "missing_final=${PARTIAL_CHECKSUMS}" "${PUBLICATION_FAILURE_LOG}"
grep -Fqx "missing_final=${PARTIAL_SIGNATURE}" "${PUBLICATION_FAILURE_LOG}"
[[ "$(grep -c '^matching_final=' "${PUBLICATION_FAILURE_LOG}")" == "1" ]]
[[ "$(grep -c '^missing_final=' "${PUBLICATION_FAILURE_LOG}")" == "2" ]]

EXPECTED_PARTIAL_ARCHIVE="${TEST_ROOT}/expected-partial-archive"
cp "${PARTIAL_ARCHIVE}" "${EXPECTED_PARTIAL_ARCHIVE}"
DIVERGENT_ARCHIVE="${TEST_ROOT}/divergent-archive"
DIVERGENT_ARCHIVE_COPY="${TEST_ROOT}/divergent-archive-copy"
printf 'divergent pre-existing release asset\n' >"${DIVERGENT_ARCHIVE}"
mv "${DIVERGENT_ARCHIVE}" "${PARTIAL_ARCHIVE}"
cp "${PARTIAL_ARCHIVE}" "${DIVERGENT_ARCHIVE_COPY}"
DIVERGENT_FAILURE_LOG="${TEST_ROOT}/divergent-failure.log"
if run_test_release_builder "${PUBLICATION_FAILURE_DIRECTORY}" >"${DIVERGENT_FAILURE_LOG}" 2>&1; then
  printf 'Release builder accepted a divergent pre-existing asset\n' >&2
  exit 1
fi
cmp -s "${DIVERGENT_ARCHIVE_COPY}" "${PARTIAL_ARCHIVE}"
grep -Fqx "divergent_final=${PARTIAL_ARCHIVE}" "${DIVERGENT_FAILURE_LOG}"
grep -Fqx "missing_final=${PARTIAL_CHECKSUMS}" "${DIVERGENT_FAILURE_LOG}"
grep -Fqx "missing_final=${PARTIAL_SIGNATURE}" "${DIVERGENT_FAILURE_LOG}"

RESTORED_ARCHIVE="${TEST_ROOT}/restored-partial-archive"
cp "${EXPECTED_PARTIAL_ARCHIVE}" "${RESTORED_ARCHIVE}"
mv "${RESTORED_ARCHIVE}" "${PARTIAL_ARCHIVE}"
run_test_release_builder "${PUBLICATION_FAILURE_DIRECTORY}"
cmp -s "${EXPECTED_PARTIAL_ARCHIVE}" "${PARTIAL_ARCHIVE}"
[[ -f "${PARTIAL_CHECKSUMS}" && -f "${PARTIAL_SIGNATURE}" ]]
[[ ! -e "${PARTIAL_STAGE}" && ! -L "${PARTIAL_STAGE}" ]]
(
  cd "${PUBLICATION_FAILURE_DIRECTORY}"
  shasum -a 256 -c SHA256SUMS
)
go run -mod=readonly "${MAILCLI_ROOT}/cmd/mailcli-release-sign" verify \
  --public "${TEST_PUBLIC_KEY}" \
  --input "${PARTIAL_CHECKSUMS}" \
  --signature "${PARTIAL_SIGNATURE}"

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
cmp -s "${PACKAGE_ROOT}/bin/mailcli" "${BUILD_OUTPUT}"
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

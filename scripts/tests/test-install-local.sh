#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)}"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
check_go_toolchain "${MAILCLI_ROOT}"
GOMODCACHE_ROOT="$(go env GOMODCACHE)"
GOCACHE_ROOT="$(go env GOCACHE)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-local-install-test.XXXXXX")"
TEST_HOME="${TEST_ROOT}/home"
BUILD_OUTPUT="${TEST_ROOT}/build/mailcli"
SOURCE_ROOT="${TEST_ROOT}/source"
BINARY_DESTINATION="${TEST_ROOT}/install/.local/bin/mailcli"
SKILL_DESTINATION="${TEST_ROOT}/install/.agents/skills/mailcli"
TRANSACTION_ROOT="${TEST_HOME}/Library/Application Support/MailCLI/install-transactions"
LOCAL_INSTALL_PROCESS=""

cleanup() {
  if [[ -n "${LOCAL_INSTALL_PROCESS}" ]]; then
    exec 9>&-
    wait "${LOCAL_INSTALL_PROCESS}" || true
  fi
  if [[ "${TEST_ROOT}" == *"/mailcli-local-install-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "${TEST_HOME}"

repository_binary_state() {
  local binary="${MAILCLI_ROOT}/bin/mailcli"
  if [[ -L "${binary}" ]]; then
    printf 'symlink:%s\n' "$(readlink "${binary}")"
  elif [[ -f "${binary}" ]]; then
    shasum -a 256 "${binary}"
  elif [[ -e "${binary}" ]]; then
    printf 'other\n'
  else
    printf 'missing\n'
  fi
}
REPOSITORY_BINARY_BEFORE="$(repository_binary_state)"
mkdir -p "${SOURCE_ROOT}/scripts/build" "${SOURCE_ROOT}/scripts/release" \
  "${SOURCE_ROOT}/scripts/utils" "${SOURCE_ROOT}/bin"
cp "${MAILCLI_ROOT}/go.mod" "${MAILCLI_ROOT}/go.sum" "${SOURCE_ROOT}/"
cp -R "${MAILCLI_ROOT}/cmd" "${MAILCLI_ROOT}/internal" "${MAILCLI_ROOT}/skills" "${SOURCE_ROOT}/"
cp "${MAILCLI_ROOT}/scripts/build/build.sh" "${MAILCLI_ROOT}/scripts/build/install-local.sh" "${SOURCE_ROOT}/scripts/build/"
cp "${MAILCLI_ROOT}/scripts/release/install.sh" "${SOURCE_ROOT}/scripts/release/"
cp "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh" "${SOURCE_ROOT}/scripts/utils/"

install_local() (
  local binary_destination="$1"
  local skill_destination="$2"
  cd "${SOURCE_ROOT}"
  HOME="${TEST_HOME}" GOMODCACHE="${GOMODCACHE_ROOT}" GOCACHE="${GOCACHE_ROOT}" \
    MAILCLI_OUTPUT=json \
    MAILCLI_SKILL_DESTINATION="${skill_destination}" \
    MAILCLI_BUILD_OUTPUT="${BUILD_OUTPUT}" \
    "${SOURCE_ROOT}/scripts/build/install-local.sh" "${binary_destination}" >/dev/null
)

verify_install() {
  local binary_destination="$1"
  local skill_destination="$2"
  cmp -s "${BUILD_OUTPUT}" "${binary_destination}"
  if ! otool -l "${binary_destination}" |
    awk '$1 == "cmd" && $2 == "LC_UUID" { count++ } END { exit(count != 1) }'; then
    printf 'Installed binary must retain exactly one Mach-O LC_UUID\n' >&2
    return 1
  fi
  diff -qr "${MAILCLI_ROOT}/skills/mailcli" "${skill_destination}" >/dev/null
  (
    cd "${MAILCLI_ROOT}"
    MAILCLI_TEST_SKILL_DIRECTORY="${skill_destination}" \
      go test ./internal/cli -run '^TestSkillDocumentationSelfContained$' -count=1
  )
  [[ "$(MAILCLI_OUTPUT=human "${binary_destination}" version)" == "$(MAILCLI_OUTPUT=human "${BUILD_OUTPUT}" version)" ]]
  [[ ! -e "${binary_destination}.mailcli-backup" ]]
  [[ ! -e "${skill_destination}.mailcli-backup" ]]
}

# Regression: a pre-existing ignored production binary must keep its digest
# while the installer path builds and installs the redirected candidate.
PREEXISTING_BINARY="${SOURCE_ROOT}/bin/mailcli"
printf 'pre-existing binary sentinel\n' >"${PREEXISTING_BINARY}"
PREEXISTING_DIGEST="$(shasum -a 256 "${PREEXISTING_BINARY}" | awk '{print $1}')"
if [[ "${MAILCLI_TEST_FAIL_AFTER_SENTINEL:-}" == 1 ]]; then
  printf 'Injected failure after private sentinel creation\n' >&2
  exit 73
fi
mkdir "${TEST_ROOT}/failure-tmp"
FAILURE_STATUS=0
TMPDIR="${TEST_ROOT}/failure-tmp" MAILCLI_TEST_FAIL_AFTER_SENTINEL=1 \
  bash "${BASH_SOURCE[0]}" >"${TEST_ROOT}/failure.log" 2>&1 || FAILURE_STATUS=$?
[[ "${FAILURE_STATUS}" == 73 ]]
grep -Fqx 'Injected failure after private sentinel creation' "${TEST_ROOT}/failure.log"
[[ -z "$(find "${TEST_ROOT}/failure-tmp" -mindepth 1 -print -quit)" ]]
[[ "$(repository_binary_state)" == "${REPOSITORY_BINARY_BEFORE}" ]]

install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
verify_install "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"

[[ "$(shasum -a 256 "${PREEXISTING_BINARY}" | awk '{print $1}')" == "${PREEXISTING_DIGEST}" ]] || {
  printf 'Installer build changed the pre-existing bin/mailcli digest\n' >&2
  exit 1
}
[[ "$(repository_binary_state)" == "${REPOSITORY_BINARY_BEFORE}" ]]

printf 'old binary\n' >"${BINARY_DESTINATION}"
chmod 0755 "${BINARY_DESTINATION}"
printf 'old skill\n' >"${SKILL_DESTINATION}/SKILL.md"
# Prove the real source wrapper reaches the shared installer and waits on the
# updater's persistent lock before replacing either destination.
WAIT_ENV="${TEST_ROOT}/record-installer-entry.sh"
# shellcheck disable=SC2016
printf '%s\n' \
  'if [[ "$0" == */scripts/release/install.sh ]]; then' \
  '  : > "$MAILCLI_TEST_INSTALLER_ENTERED"' \
  'fi' >"${WAIT_ENV}"
exec 9<>"${TEST_HOME}/Library/Application Support/MailCLI/update.lock"
/usr/bin/lockf -s -t 0 9
(
  exec 9>&-
  BASH_ENV="${WAIT_ENV}" MAILCLI_TEST_INSTALLER_ENTERED="${TEST_ROOT}/installer-entered" \
    install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
) &
LOCAL_INSTALL_PROCESS=$!
WAIT_DEADLINE=$((SECONDS + 30))
while [[ ! -f "${TEST_ROOT}/installer-entered" ]]; do
  if ((SECONDS >= WAIT_DEADLINE)) || ! kill -0 "${LOCAL_INSTALL_PROCESS}" 2>/dev/null; then
    printf 'Local installer did not reach the shared installation boundary\n' >&2
    exit 1
  fi
  sleep 0.05
done
sleep 0.2
kill -0 "${LOCAL_INSTALL_PROCESS}"
[[ "$(<"${BINARY_DESTINATION}")" == 'old binary' ]]
[[ "$(<"${SKILL_DESTINATION}/SKILL.md")" == 'old skill' ]]
[[ -z "$(find "${TRANSACTION_ROOT}" -mindepth 1 -maxdepth 1 -print -quit)" ]]
exec 9>&-
wait "${LOCAL_INSTALL_PROCESS}"
LOCAL_INSTALL_PROCESS=""
verify_install "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"

cp "${BINARY_DESTINATION}" "${TEST_ROOT}/binary-before-backup-refusal"
cp -R "${SKILL_DESTINATION}" "${TEST_ROOT}/skill-before-backup-refusal"
mkdir "${BINARY_DESTINATION}.mailcli-backup"
if install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"; then
  printf 'Local installer unexpectedly replaced content while a backup path existed\n' >&2
  exit 1
fi
cmp -s "${TEST_ROOT}/binary-before-backup-refusal" "${BINARY_DESTINATION}"
diff -qr "${TEST_ROOT}/skill-before-backup-refusal" "${SKILL_DESTINATION}" >/dev/null
rmdir "${BINARY_DESTINATION}.mailcli-backup"

SYMLINK_ROOT="${TEST_ROOT}/symlink-check"
mkdir -p "${SYMLINK_ROOT}/outside" "${SYMLINK_ROOT}/safe"
ln -s "${SYMLINK_ROOT}/outside" "${SYMLINK_ROOT}/linked-parent"
if install_local "${SYMLINK_ROOT}/linked-parent/mailcli" "${SYMLINK_ROOT}/safe/skill"; then
  printf 'Local installer accepted a symbolic-link parent directory\n' >&2
  exit 1
fi
[[ ! -e "${SYMLINK_ROOT}/outside/mailcli" ]]
ln -s "${SYMLINK_ROOT}/outside/missing" "${SYMLINK_ROOT}/linked-destination"
if install_local "${SYMLINK_ROOT}/linked-destination" "${SYMLINK_ROOT}/safe/skill"; then
  printf 'Local installer accepted a symbolic-link destination\n' >&2
  exit 1
fi
[[ ! -e "${SYMLINK_ROOT}/outside/missing" ]]

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

printf 'rollback binary\n' >"${BINARY_DESTINATION}"
chmod 0755 "${BINARY_DESTINATION}"
printf 'rollback skill\n' >"${SKILL_DESTINATION}/SKILL.md"
set +e
(
cd "${SOURCE_ROOT}"
MAILCLI_TEST_INTERRUPT_PATH="${BINARY_DESTINATION}" BASH_ENV="${INTERRUPT_ENV}" \
  HOME="${TEST_HOME}" GOMODCACHE="${GOMODCACHE_ROOT}" GOCACHE="${GOCACHE_ROOT}" \
  MAILCLI_SKILL_DESTINATION="${SKILL_DESTINATION}" \
  MAILCLI_BUILD_OUTPUT="${BUILD_OUTPUT}" \
  "${SOURCE_ROOT}/scripts/build/install-local.sh" "${BINARY_DESTINATION}" >/dev/null 2>&1
)
INTERRUPTION_STATUS=$?
set -e
[[ "${INTERRUPTION_STATUS}" -eq 137 ]]
find "${TRANSACTION_ROOT}" -maxdepth 1 -type d -name 'txn.*' -print -quit | grep -q .
install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
verify_install "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"

printf 'rollback binary again\n' >"${BINARY_DESTINATION}"
chmod 0755 "${BINARY_DESTINATION}"
printf 'rollback skill again\n' >"${SKILL_DESTINATION}/SKILL.md"
set +e
(
cd "${SOURCE_ROOT}"
MAILCLI_TEST_INTERRUPT_PATH="${SKILL_DESTINATION}" BASH_ENV="${INTERRUPT_ENV}" \
  HOME="${TEST_HOME}" GOMODCACHE="${GOMODCACHE_ROOT}" GOCACHE="${GOCACHE_ROOT}" \
  MAILCLI_SKILL_DESTINATION="${SKILL_DESTINATION}" \
  MAILCLI_BUILD_OUTPUT="${BUILD_OUTPUT}" \
  "${SOURCE_ROOT}/scripts/build/install-local.sh" "${BINARY_DESTINATION}" >/dev/null 2>&1
)
INTERRUPTION_STATUS=$?
set -e
[[ "${INTERRUPTION_STATUS}" -eq 137 ]]
find "${TRANSACTION_ROOT}" -maxdepth 1 -type d -name 'txn.*' -print -quit | grep -q .
install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
verify_install "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"

[[ "$(repository_binary_state)" == "${REPOSITORY_BINARY_BEFORE}" ]]
printf 'Local source installation tests passed\n'

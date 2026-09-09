#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
GOMODCACHE_ROOT="$(go env GOMODCACHE)"
GOCACHE_ROOT="$(go env GOCACHE)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-local-install-test.XXXXXX")"
TEST_HOME="${TEST_ROOT}/home"
BINARY_DESTINATION="${TEST_ROOT}/install/.local/bin/mailcli"
SKILL_DESTINATION="${TEST_ROOT}/install/.agents/skills/mailcli"
TRANSACTION_ROOT="${TEST_HOME}/Library/Application Support/MailCLI/install-transactions"

cleanup() {
  if [[ "${TEST_ROOT}" == *"/mailcli-local-install-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup EXIT
mkdir -p "${TEST_HOME}"

install_local() {
  local binary_destination="$1"
  local skill_destination="$2"
  HOME="${TEST_HOME}" GOMODCACHE="${GOMODCACHE_ROOT}" GOCACHE="${GOCACHE_ROOT}" \
    MAILCLI_SKILL_DESTINATION="${skill_destination}" \
    "${MAILCLI_ROOT}/scripts/build/install-local.sh" "${binary_destination}" >/dev/null
}

verify_install() {
  local binary_destination="$1"
  local skill_destination="$2"
  cmp -s "${MAILCLI_ROOT}/bin/mailcli" "${binary_destination}"
  diff -qr "${MAILCLI_ROOT}/skills/mailcli" "${skill_destination}" >/dev/null
  [[ "$(${binary_destination} version)" == "$("${MAILCLI_ROOT}/bin/mailcli" version)" ]]
  [[ ! -e "${binary_destination}.mailcli-backup" ]]
  [[ ! -e "${skill_destination}.mailcli-backup" ]]
}

install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
verify_install "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"

printf 'old binary\n' >"${BINARY_DESTINATION}"
chmod 0755 "${BINARY_DESTINATION}"
printf 'old skill\n' >"${SKILL_DESTINATION}/SKILL.md"
install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
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
MAILCLI_TEST_INTERRUPT_PATH="${BINARY_DESTINATION}" BASH_ENV="${INTERRUPT_ENV}" \
  HOME="${TEST_HOME}" GOMODCACHE="${GOMODCACHE_ROOT}" GOCACHE="${GOCACHE_ROOT}" \
  MAILCLI_SKILL_DESTINATION="${SKILL_DESTINATION}" \
  "${MAILCLI_ROOT}/scripts/build/install-local.sh" "${BINARY_DESTINATION}" >/dev/null 2>&1
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
MAILCLI_TEST_INTERRUPT_PATH="${SKILL_DESTINATION}" BASH_ENV="${INTERRUPT_ENV}" \
  HOME="${TEST_HOME}" GOMODCACHE="${GOMODCACHE_ROOT}" GOCACHE="${GOCACHE_ROOT}" \
  MAILCLI_SKILL_DESTINATION="${SKILL_DESTINATION}" \
  "${MAILCLI_ROOT}/scripts/build/install-local.sh" "${BINARY_DESTINATION}" >/dev/null 2>&1
INTERRUPTION_STATUS=$?
set -e
[[ "${INTERRUPTION_STATUS}" -eq 137 ]]
find "${TRANSACTION_ROOT}" -maxdepth 1 -type d -name 'txn.*' -print -quit | grep -q .
install_local "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"
verify_install "${BINARY_DESTINATION}" "${SKILL_DESTINATION}"

printf 'Local source installation tests passed\n'

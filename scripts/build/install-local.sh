#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_SOURCE="${MAILCLI_ROOT}/bin/mailcli"
MAILCLI_DESTINATION="${1:-${HOME}/.local/bin/mailcli}"

"${MAILCLI_ROOT}/scripts/build/build.sh"
mkdir -p "$(dirname "${MAILCLI_DESTINATION}")"

if [[ -e "${MAILCLI_DESTINATION}" ]]; then
  if cmp -s "${MAILCLI_SOURCE}" "${MAILCLI_DESTINATION}"; then
    printf 'MailCLI is already installed at %s\n' "${MAILCLI_DESTINATION}"
    exit 0
  fi
  MAILCLI_BACKUP="${MAILCLI_DESTINATION}.mailcli-backup"
  if [[ -e "${MAILCLI_BACKUP}" ]]; then
    printf 'Refusing install because backup path exists: %s\n' "${MAILCLI_BACKUP}" >&2
    exit 1
  fi
  mv "${MAILCLI_DESTINATION}" "${MAILCLI_BACKUP}"
fi

if ! cp "${MAILCLI_SOURCE}" "${MAILCLI_DESTINATION}"; then
  if [[ -n "${MAILCLI_BACKUP:-}" ]]; then
    mv "${MAILCLI_BACKUP}" "${MAILCLI_DESTINATION}"
  fi
  exit 1
fi
chmod 0755 "${MAILCLI_DESTINATION}"
if ! cmp -s "${MAILCLI_SOURCE}" "${MAILCLI_DESTINATION}"; then
  rm "${MAILCLI_DESTINATION}"
  if [[ -n "${MAILCLI_BACKUP:-}" ]]; then
    mv "${MAILCLI_BACKUP}" "${MAILCLI_DESTINATION}"
  fi
  printf 'Installed binary verification failed\n' >&2
  exit 1
fi
if [[ -n "${MAILCLI_BACKUP:-}" ]]; then
  rm "${MAILCLI_BACKUP}"
fi
printf 'Installed MailCLI at %s\n' "${MAILCLI_DESTINATION}"

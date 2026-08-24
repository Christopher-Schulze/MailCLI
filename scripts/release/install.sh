#!/usr/bin/env bash
set -euo pipefail

PACKAGE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_BINARY="${PACKAGE_ROOT}/bin/mailcli"
SOURCE_SKILL="${PACKAGE_ROOT}/skills/mailcli"
BINARY_DESTINATION="${MAILCLI_BINARY_DESTINATION:-${HOME}/.local/bin/mailcli}"
SKILL_DESTINATION="${MAILCLI_SKILL_DESTINATION:-${HOME}/.agents/skills/mailcli}"
BINARY_BACKUP="${BINARY_DESTINATION}.mailcli-backup"
SKILL_BACKUP="${SKILL_DESTINATION}.mailcli-backup"

validate_destination() {
  local label="$1"
  local path="$2"
  if [[ "${path}" != /* || "${path}" == "/" || "${path}" == "${HOME}" ]]; then
    printf '%s destination must be a safe absolute path: %s\n' "${label}" "${path}" >&2
    exit 1
  fi
}

validate_destination "Binary" "${BINARY_DESTINATION}"
validate_destination "Skill" "${SKILL_DESTINATION}"
if [[ "${BINARY_DESTINATION}" == */../* || "${BINARY_DESTINATION}" == */.. ||
  "${SKILL_DESTINATION}" == */../* || "${SKILL_DESTINATION}" == */.. ]]; then
  printf 'Install destinations must not contain parent traversal\n' >&2
  exit 1
fi
paths_overlap() {
  local left="$1"
  local right="$2"
  [[ "${left}" == "${right}" || "${left}/" == "${right}/"* || "${right}/" == "${left}/"* ]]
}

INSTALL_PATHS=("${BINARY_DESTINATION}" "${SKILL_DESTINATION}" "${BINARY_BACKUP}" "${SKILL_BACKUP}")
for ((LEFT_INDEX = 0; LEFT_INDEX < ${#INSTALL_PATHS[@]}; LEFT_INDEX++)); do
  for ((RIGHT_INDEX = LEFT_INDEX + 1; RIGHT_INDEX < ${#INSTALL_PATHS[@]}; RIGHT_INDEX++)); do
    if paths_overlap "${INSTALL_PATHS[LEFT_INDEX]}" "${INSTALL_PATHS[RIGHT_INDEX]}"; then
      printf 'Binary, skill, and backup paths must not overlap\n' >&2
      exit 1
    fi
  done
done

if [[ ! -x "${SOURCE_BINARY}" ]]; then
  printf 'Release binary is missing or not executable: %s\n' "${SOURCE_BINARY}" >&2
  exit 1
fi
if [[ ! -f "${SOURCE_SKILL}/SKILL.md" || ! -f "${SOURCE_SKILL}/agents/openai.yaml" ]]; then
  printf 'Release skill is incomplete: %s\n' "${SOURCE_SKILL}" >&2
  exit 1
fi
if find "${SOURCE_SKILL}" -type l -print -quit | grep -q .; then
  printf 'Release skill must not contain symbolic links\n' >&2
  exit 1
fi
if [[ -L "${BINARY_DESTINATION}" || -L "${SKILL_DESTINATION}" ]]; then
  printf 'Refusing to replace a symbolic-link destination\n' >&2
  exit 1
fi
if [[ -e "${BINARY_BACKUP}" || -e "${SKILL_BACKUP}" ]]; then
  printf 'Refusing install because a backup path already exists\n' >&2
  exit 1
fi

BINARY_PARENT="$(dirname "${BINARY_DESTINATION}")"
SKILL_PARENT="$(dirname "${SKILL_DESTINATION}")"
mkdir -p "${BINARY_PARENT}" "${SKILL_PARENT}"

BINARY_STAGE=""
SKILL_STAGE=""
BINARY_BACKED_UP=0
SKILL_BACKED_UP=0
BINARY_INSTALLED=0
SKILL_INSTALLED=0
INSTALL_COMPLETE=0

rollback_install() {
  local status=$?
  set +e
  if [[ "${INSTALL_COMPLETE}" -ne 1 ]]; then
    if [[ "${BINARY_INSTALLED}" -eq 1 ]]; then
      rm -f "${BINARY_DESTINATION}"
    fi
    if [[ "${SKILL_INSTALLED}" -eq 1 ]]; then
      rm -rf "${SKILL_DESTINATION}"
    fi
    if [[ "${BINARY_BACKED_UP}" -eq 1 ]]; then
      mv "${BINARY_BACKUP}" "${BINARY_DESTINATION}"
    fi
    if [[ "${SKILL_BACKED_UP}" -eq 1 ]]; then
      mv "${SKILL_BACKUP}" "${SKILL_DESTINATION}"
    fi
  fi
  [[ -z "${BINARY_STAGE}" || ! -e "${BINARY_STAGE}" ]] || rm -f "${BINARY_STAGE}"
  [[ -z "${SKILL_STAGE}" || ! -e "${SKILL_STAGE}" ]] || rm -rf "${SKILL_STAGE}"
  exit "${status}"
}
trap rollback_install EXIT

BINARY_STAGE="$(mktemp "${BINARY_PARENT}/.mailcli-binary.XXXXXX")"
SKILL_STAGE="$(mktemp -d "${SKILL_PARENT}/.mailcli-skill.XXXXXX")"
cp "${SOURCE_BINARY}" "${BINARY_STAGE}"
chmod 0755 "${BINARY_STAGE}"
cmp -s "${SOURCE_BINARY}" "${BINARY_STAGE}"
cp -R "${SOURCE_SKILL}/." "${SKILL_STAGE}/"
diff -qr "${SOURCE_SKILL}" "${SKILL_STAGE}" >/dev/null

if [[ -e "${BINARY_DESTINATION}" ]]; then
  mv "${BINARY_DESTINATION}" "${BINARY_BACKUP}"
  BINARY_BACKED_UP=1
fi
if [[ -e "${SKILL_DESTINATION}" ]]; then
  mv "${SKILL_DESTINATION}" "${SKILL_BACKUP}"
  SKILL_BACKED_UP=1
fi

mv "${BINARY_STAGE}" "${BINARY_DESTINATION}"
BINARY_INSTALLED=1
mv "${SKILL_STAGE}" "${SKILL_DESTINATION}"
SKILL_INSTALLED=1

cmp -s "${SOURCE_BINARY}" "${BINARY_DESTINATION}"
diff -qr "${SOURCE_SKILL}" "${SKILL_DESTINATION}" >/dev/null

INSTALL_COMPLETE=1
if [[ "${BINARY_BACKED_UP}" -eq 1 ]]; then
  rm -f "${BINARY_BACKUP}"
fi
if [[ "${SKILL_BACKED_UP}" -eq 1 ]]; then
  rm -rf "${SKILL_BACKUP}"
fi

printf 'Installed MailCLI binary at %s\n' "${BINARY_DESTINATION}"
printf 'Installed MailCLI skill at %s\n' "${SKILL_DESTINATION}"
printf 'Start a new agent session to load the installed skill.\n'

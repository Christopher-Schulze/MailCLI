#!/usr/bin/env bash
set -euo pipefail

PACKAGE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_BINARY="${PACKAGE_ROOT}/bin/mailcli"
SOURCE_SKILL="${PACKAGE_ROOT}/skills/mailcli"
BINARY_DESTINATION="${MAILCLI_BINARY_DESTINATION:-${HOME}/.local/bin/mailcli}"
SKILL_DESTINATION="${MAILCLI_SKILL_DESTINATION:-${HOME}/.agents/skills/mailcli}"
BINARY_BACKUP="${BINARY_DESTINATION}.mailcli-backup"
SKILL_BACKUP="${SKILL_DESTINATION}.mailcli-backup"
INSTALL_STATE_ROOT="${HOME}/Library/Application Support/MailCLI/install-transactions"

TRANSACTION_DIRECTORY=""
TRANSACTION_MANIFEST=""
BINARY_STAGE=""
SKILL_STAGE=""
BINARY_SNAPSHOT=""
SKILL_SNAPSHOT=""
BINARY_STAGE_IDENTITY=""
SKILL_STAGE_IDENTITY=""
BINARY_HAD_DESTINATION=0
SKILL_HAD_DESTINATION=0
TRANSACTION_STATE=""

validate_destination() {
  local label="$1"
  local path="$2"
  if [[ "${path}" != /* || "${path}" == "/" || "${path}" == "${HOME}" ||
    "${path}" == *$'\t'* || "${path}" == *$'\n'* || "${path}" == *$'\r'* ]]; then
    printf '%s destination must be a safe absolute path: %s\n' "${label}" "${path}" >&2
    exit 1
  fi
}

path_present() {
  [[ -e "$1" || -L "$1" ]]
}

trusted_system_symlink() {
  case "$1" in
    /tmp) [[ "$(readlink "$1" 2>/dev/null)" == "private/tmp" ]] ;;
    /var) [[ "$(readlink "$1" 2>/dev/null)" == "private/var" ]] ;;
    *) return 1 ;;
  esac
}

directory_is_safe() {
  [[ -d "$1" ]] || return 1
  if [[ -L "$1" ]] && ! trusted_system_symlink "$1"; then
    return 1
  fi
}

ensure_directory_chain() {
  local path="$1"
  local current="/"
  local component
  local -a components
  if [[ "${path}" != /* || "${path}" == "/" ]]; then
    printf 'Directory path must be a safe absolute path: %s\n' "${path}" >&2
    return 1
  fi
  IFS='/' read -r -a components <<< "${path#/}"
  for component in "${components[@]}"; do
    [[ -z "${component}" ]] && continue
    current="${current%/}/${component}"
    if [[ -L "${current}" ]] && ! trusted_system_symlink "${current}"; then
      printf 'Directory path contains a symbolic link: %s\n' "${current}" >&2
      return 1
    fi
    if [[ -e "${current}" ]]; then
      if [[ ! -d "${current}" ]]; then
        printf 'Directory path is not a directory: %s\n' "${current}" >&2
        return 1
      fi
    elif ! mkdir "${current}"; then
      printf 'Create directory failed: %s\n' "${current}" >&2
      return 1
    fi
    if ! directory_is_safe "${current}"; then
      printf 'Directory changed unsafely: %s\n' "${current}" >&2
      return 1
    fi
  done
}

validate_existing_directory_chain() {
  local path="$1"
  local current="/"
  local component
  local -a components
  if [[ "${path}" != /* || "${path}" == "/" ]]; then
    return 1
  fi
  IFS='/' read -r -a components <<< "${path#/}"
  for component in "${components[@]}"; do
    [[ -z "${component}" ]] && continue
    current="${current%/}/${component}"
    if ! directory_is_safe "${current}"; then
      return 1
    fi
  done
}

sync_filesystem() {
  sync
}

path_identity() {
  stat -f '%d:%i' "$1"
}

manifest_path_is_safe() {
  local path="$1"
  [[ "${path}" == /* && "${path}" != "/" ]] || return 1
  [[ "${path}" != *$'\t'* && "${path}" != *$'\n'* && "${path}" != *$'\r'* ]] || return 1
  [[ "${path}" != */../* && "${path}" != */.. ]] || return 1
}

manifest_write() {
  local state="$1"
  local temporary="${TRANSACTION_DIRECTORY}/manifest.tmp"
  {
    printf 'mailcli-install-v1\n'
    printf 'state\t%s\n' "${state}"
    printf 'binary_destination\t%s\n' "${BINARY_DESTINATION}"
    printf 'skill_destination\t%s\n' "${SKILL_DESTINATION}"
    printf 'binary_stage\t%s\n' "${BINARY_STAGE}"
    printf 'skill_stage\t%s\n' "${SKILL_STAGE}"
    printf 'binary_stage_identity\t%s\n' "${BINARY_STAGE_IDENTITY}"
    printf 'skill_stage_identity\t%s\n' "${SKILL_STAGE_IDENTITY}"
    printf 'binary_had_destination\t%s\n' "${BINARY_HAD_DESTINATION}"
    printf 'skill_had_destination\t%s\n' "${SKILL_HAD_DESTINATION}"
  } >"${temporary}" || return 1
  chmod 0600 "${temporary}" || return 1
  mv -f "${temporary}" "${TRANSACTION_MANIFEST}" || return 1
  TRANSACTION_STATE="${state}"
  sync_filesystem
}

read_manifest() {
  local transaction="$1"
  local header key value extra
  local have_state=0
  local have_binary_destination=0
  local have_skill_destination=0
  local have_binary_stage=0
  local have_skill_stage=0
  local have_binary_stage_identity=0
  local have_skill_stage_identity=0
  local have_binary_had=0
  local have_skill_had=0
  TRANSACTION_DIRECTORY="${transaction}"
  TRANSACTION_MANIFEST="${transaction}/manifest"
  BINARY_STAGE=""
  SKILL_STAGE=""
  BINARY_STAGE_IDENTITY=""
  SKILL_STAGE_IDENTITY=""
  BINARY_HAD_DESTINATION=0
  SKILL_HAD_DESTINATION=0
  TRANSACTION_STATE=""
  [[ -f "${TRANSACTION_MANIFEST}" && ! -L "${TRANSACTION_MANIFEST}" ]] || return 1
  IFS= read -r header <"${TRANSACTION_MANIFEST}" || return 1
  [[ "${header}" == "mailcli-install-v1" ]] || return 1
  while IFS=$'\t' read -r key value extra; do
    if [[ "${key}" == "mailcli-install-v1" && -z "${value}" && -z "${extra}" ]]; then
      continue
    fi
    [[ -z "${extra}" ]] || return 1
    case "${key}" in
      state)
        [[ "${have_state}" -eq 0 ]] || return 1
        TRANSACTION_STATE="${value}"
        have_state=1
        ;;
      binary_destination)
        [[ "${have_binary_destination}" -eq 0 ]] || return 1
        BINARY_DESTINATION="${value}"
        have_binary_destination=1
        ;;
      skill_destination)
        [[ "${have_skill_destination}" -eq 0 ]] || return 1
        SKILL_DESTINATION="${value}"
        have_skill_destination=1
        ;;
      binary_stage)
        [[ "${have_binary_stage}" -eq 0 ]] || return 1
        BINARY_STAGE="${value}"
        have_binary_stage=1
        ;;
      skill_stage)
        [[ "${have_skill_stage}" -eq 0 ]] || return 1
        SKILL_STAGE="${value}"
        have_skill_stage=1
        ;;
      binary_stage_identity)
        [[ "${have_binary_stage_identity}" -eq 0 ]] || return 1
        BINARY_STAGE_IDENTITY="${value}"
        have_binary_stage_identity=1
        ;;
      skill_stage_identity)
        [[ "${have_skill_stage_identity}" -eq 0 ]] || return 1
        SKILL_STAGE_IDENTITY="${value}"
        have_skill_stage_identity=1
        ;;
      binary_had_destination)
        [[ "${have_binary_had}" -eq 0 ]] || return 1
        BINARY_HAD_DESTINATION="${value}"
        have_binary_had=1
        ;;
      skill_had_destination)
        [[ "${have_skill_had}" -eq 0 ]] || return 1
        SKILL_HAD_DESTINATION="${value}"
        have_skill_had=1
        ;;
      *)
        return 1
        ;;
    esac
  done <"${TRANSACTION_MANIFEST}"
  [[ "${have_state}" -eq 1 && "${have_binary_destination}" -eq 1 &&
    "${have_skill_destination}" -eq 1 && "${have_binary_stage}" -eq 1 &&
    "${have_skill_stage}" -eq 1 && "${have_binary_stage_identity}" -eq 1 &&
    "${have_skill_stage_identity}" -eq 1 && "${have_binary_had}" -eq 1 &&
    "${have_skill_had}" -eq 1 ]] || return 1
  BINARY_BACKUP="${BINARY_DESTINATION}.mailcli-backup"
  SKILL_BACKUP="${SKILL_DESTINATION}.mailcli-backup"
  BINARY_SNAPSHOT="${TRANSACTION_DIRECTORY}/binary.snapshot"
  SKILL_SNAPSHOT="${TRANSACTION_DIRECTORY}/skill.snapshot"
}

validate_transaction() {
  local transaction_name
  local binary_parent
  local skill_parent
  transaction_name="$(basename "${TRANSACTION_DIRECTORY}")"
  [[ "${TRANSACTION_DIRECTORY}" == "${INSTALL_STATE_ROOT}"/txn.* ]] || return 1
  [[ -d "${TRANSACTION_DIRECTORY}" && ! -L "${TRANSACTION_DIRECTORY}" ]] || return 1
  [[ "${TRANSACTION_STATE}" == "prepared" ||
    "${TRANSACTION_STATE}" == "binary_backup_pending" ||
    "${TRANSACTION_STATE}" == "binary_backed_up" ||
    "${TRANSACTION_STATE}" == "skill_backup_pending" ||
    "${TRANSACTION_STATE}" == "skill_backed_up" ||
    "${TRANSACTION_STATE}" == "binary_install_pending" ||
    "${TRANSACTION_STATE}" == "binary_installed" ||
    "${TRANSACTION_STATE}" == "skill_install_pending" ||
    "${TRANSACTION_STATE}" == "skill_installed" ||
    "${TRANSACTION_STATE}" == "verified" ||
    "${TRANSACTION_STATE}" == "committed" ]] || return 1
  [[ "${BINARY_HAD_DESTINATION}" == 0 || "${BINARY_HAD_DESTINATION}" == 1 ]] || return 1
  [[ "${SKILL_HAD_DESTINATION}" == 0 || "${SKILL_HAD_DESTINATION}" == 1 ]] || return 1
  manifest_path_is_safe "${BINARY_DESTINATION}" || return 1
  manifest_path_is_safe "${SKILL_DESTINATION}" || return 1
  manifest_path_is_safe "${BINARY_STAGE}" || return 1
  manifest_path_is_safe "${SKILL_STAGE}" || return 1
  binary_parent="$(dirname "${BINARY_DESTINATION}")"
  skill_parent="$(dirname "${SKILL_DESTINATION}")"
  validate_existing_directory_chain "${binary_parent}" || return 1
  validate_existing_directory_chain "${skill_parent}" || return 1
  [[ "${BINARY_DESTINATION}" != "${SKILL_DESTINATION}" ]] || return 1
  paths_overlap "${BINARY_DESTINATION}" "${SKILL_DESTINATION}" && return 1
  paths_overlap "${BINARY_DESTINATION}" "${INSTALL_STATE_ROOT}" && return 1
  paths_overlap "${SKILL_DESTINATION}" "${INSTALL_STATE_ROOT}" && return 1
  [[ "${BINARY_STAGE}" == "${binary_parent}/.mailcli-binary-stage.${transaction_name}" ]] || return 1
  [[ "${SKILL_STAGE}" == "${skill_parent}/.mailcli-skill-stage.${transaction_name}" ]] || return 1
  if [[ -z "${BINARY_STAGE_IDENTITY}" ]]; then
    [[ "${TRANSACTION_STATE}" == "prepared" && ! -e "${BINARY_STAGE}" && ! -L "${BINARY_STAGE}" ]] || return 1
  else
    [[ "${BINARY_STAGE_IDENTITY}" =~ ^[0-9]+:[0-9]+$ ]] || return 1
  fi
  if [[ -z "${SKILL_STAGE_IDENTITY}" ]]; then
    [[ "${TRANSACTION_STATE}" == "prepared" && ! -e "${SKILL_STAGE}" && ! -L "${SKILL_STAGE}" ]] || return 1
  else
    [[ "${SKILL_STAGE_IDENTITY}" =~ ^[0-9]+:[0-9]+$ ]] || return 1
  fi
  [[ "${BINARY_SNAPSHOT}" == "${TRANSACTION_DIRECTORY}/binary.snapshot" ]] || return 1
  [[ "${SKILL_SNAPSHOT}" == "${TRANSACTION_DIRECTORY}/skill.snapshot" ]] || return 1
}

component_matches_snapshot() {
  local component="$1"
  if [[ "${component}" == "binary" ]]; then
    [[ -f "${BINARY_DESTINATION}" && ! -L "${BINARY_DESTINATION}" &&
      -x "${BINARY_DESTINATION}" && -f "${BINARY_SNAPSHOT}" &&
      ! -L "${BINARY_SNAPSHOT}" ]] || return 1
    cmp -s "${BINARY_SNAPSHOT}" "${BINARY_DESTINATION}"
    return
  fi
  [[ -d "${SKILL_DESTINATION}" && ! -L "${SKILL_DESTINATION}" &&
    -d "${SKILL_SNAPSHOT}" && ! -L "${SKILL_SNAPSHOT}" ]] || return 1
  if find "${SKILL_DESTINATION}" -type l -print -quit | grep -q .; then
    return 1
  fi
  diff -qr "${SKILL_SNAPSHOT}" "${SKILL_DESTINATION}" >/dev/null
}

component_stage_matches_snapshot() {
  local component="$1"
  if [[ "${component}" == "binary" ]]; then
    [[ -f "${BINARY_STAGE}" && ! -L "${BINARY_STAGE}" &&
      -n "${BINARY_STAGE_IDENTITY}" &&
      "$(path_identity "${BINARY_STAGE}")" == "${BINARY_STAGE_IDENTITY}" &&
      -f "${BINARY_SNAPSHOT}" && ! -L "${BINARY_SNAPSHOT}" ]] || return 1
    cmp -s "${BINARY_SNAPSHOT}" "${BINARY_STAGE}"
    return
  fi
  [[ -d "${SKILL_STAGE}" && ! -L "${SKILL_STAGE}" &&
    -n "${SKILL_STAGE_IDENTITY}" &&
    "$(path_identity "${SKILL_STAGE}")" == "${SKILL_STAGE_IDENTITY}" &&
    -d "${SKILL_SNAPSHOT}" && ! -L "${SKILL_SNAPSHOT}" ]] || return 1
  if find "${SKILL_STAGE}" -type l -print -quit | grep -q .; then
    return 1
  fi
  diff -qr "${SKILL_SNAPSHOT}" "${SKILL_STAGE}" >/dev/null
}

remove_component_destination() {
  local component="$1"
  if [[ "${component}" == "binary" ]]; then
    rm -f "${BINARY_DESTINATION}"
  else
    rm -rf "${SKILL_DESTINATION}"
  fi
  sync_filesystem
}

remove_component_stage() {
  local component="$1"
  local stage
  if [[ "${component}" == "binary" ]]; then
    stage="${BINARY_STAGE}"
    if path_present "${stage}"; then
      [[ -f "${stage}" && ! -L "${stage}" ]] || return 1
      [[ -n "${BINARY_STAGE_IDENTITY}" && "$(path_identity "${stage}")" == "${BINARY_STAGE_IDENTITY}" ]] || return 1
      rm -f "${stage}"
    fi
  else
    stage="${SKILL_STAGE}"
    if path_present "${stage}"; then
      [[ -d "${stage}" && ! -L "${stage}" ]] || return 1
      [[ -n "${SKILL_STAGE_IDENTITY}" && "$(path_identity "${stage}")" == "${SKILL_STAGE_IDENTITY}" ]] || return 1
      if find "${stage}" -type l -print -quit | grep -q .; then
        return 1
      fi
      rm -rf "${stage}"
    fi
  fi
  sync_filesystem
}

restore_component_backup() {
  local component="$1"
  local backup
  if [[ "${component}" == "binary" ]]; then
    backup="${BINARY_BACKUP}"
    [[ -f "${backup}" && ! -L "${backup}" ]] || return 1
    mv "${backup}" "${BINARY_DESTINATION}"
    [[ -f "${BINARY_DESTINATION}" && ! -L "${BINARY_DESTINATION}" ]] || return 1
  else
    backup="${SKILL_BACKUP}"
    [[ -d "${backup}" && ! -L "${backup}" ]] || return 1
    if find "${backup}" -type l -print -quit | grep -q .; then
      return 1
    fi
    mv "${backup}" "${SKILL_DESTINATION}"
    [[ -d "${SKILL_DESTINATION}" && ! -L "${SKILL_DESTINATION}" ]] || return 1
  fi
  sync_filesystem
}

rollback_component() {
  local component="$1"
  local destination
  local backup
  local stage
  local had_destination
  if [[ "${component}" == "binary" ]]; then
    destination="${BINARY_DESTINATION}"
    backup="${BINARY_BACKUP}"
    stage="${BINARY_STAGE}"
    had_destination="${BINARY_HAD_DESTINATION}"
  else
    destination="${SKILL_DESTINATION}"
    backup="${SKILL_BACKUP}"
    stage="${SKILL_STAGE}"
    had_destination="${SKILL_HAD_DESTINATION}"
  fi
  if path_present "${backup}"; then
    [[ ! -L "${backup}" ]] || return 1
    if path_present "${destination}"; then
      component_matches_snapshot "${component}" || return 1
      remove_component_destination "${component}" || return 1
    fi
    restore_component_backup "${component}" || return 1
  elif [[ "${had_destination}" -eq 1 ]]; then
    if path_present "${destination}"; then
      [[ ! -L "${destination}" ]] || return 1
      if path_present "${stage}"; then
        :
      else
        return 1
      fi
    elif ! path_present "${stage}"; then
      return 1
    fi
  elif path_present "${destination}"; then
    component_matches_snapshot "${component}" || return 1
    remove_component_destination "${component}" || return 1
  fi
  remove_component_stage "${component}"
}

cleanup_transaction_directory() {
  rm -f "${TRANSACTION_DIRECTORY}/manifest" "${TRANSACTION_DIRECTORY}/manifest.tmp" \
    "${BINARY_SNAPSHOT}"
  if path_present "${SKILL_SNAPSHOT}"; then
    [[ -d "${SKILL_SNAPSHOT}" && ! -L "${SKILL_SNAPSHOT}" ]] || return 1
    if find "${SKILL_SNAPSHOT}" -type l -print -quit | grep -q .; then
      return 1
    fi
    rm -rf "${SKILL_SNAPSHOT}"
  fi
  rmdir "${TRANSACTION_DIRECTORY}"
  sync_filesystem
}

rollback_transaction() {
  local failed=0
  if ! rollback_component binary; then
    failed=1
  fi
  if ! rollback_component skill; then
    failed=1
  fi
  if [[ "${failed}" -ne 0 ]]; then
    return 1
  fi
  cleanup_transaction_directory
}

finalize_committed_transaction() {
  component_matches_snapshot binary || return 1
  component_matches_snapshot skill || return 1
  if path_present "${BINARY_BACKUP}"; then
    [[ -f "${BINARY_BACKUP}" && ! -L "${BINARY_BACKUP}" ]] || return 1
    rm -f "${BINARY_BACKUP}"
  fi
  if path_present "${SKILL_BACKUP}"; then
    [[ -d "${SKILL_BACKUP}" && ! -L "${SKILL_BACKUP}" ]] || return 1
    if find "${SKILL_BACKUP}" -type l -print -quit | grep -q .; then
      return 1
    fi
    rm -rf "${SKILL_BACKUP}"
  fi
  sync_filesystem
  remove_component_stage binary || return 1
  remove_component_stage skill || return 1
  cleanup_transaction_directory
}

recover_transaction() {
  local transaction="$1"
  if ! read_manifest "${transaction}" || ! validate_transaction; then
    printf 'Refusing unsafe or invalid installer transaction: %s\n' "${transaction}" >&2
    return 1
  fi
  if [[ "${TRANSACTION_STATE}" == "committed" ]]; then
    finalize_committed_transaction
  else
    rollback_transaction
  fi
}

recover_transactions() {
  local transaction
  local requested_binary_destination="${BINARY_DESTINATION}"
  local requested_skill_destination="${SKILL_DESTINATION}"
  local requested_binary_backup="${BINARY_BACKUP}"
  local requested_skill_backup="${SKILL_BACKUP}"
  local -a transactions
  shopt -s nullglob
  transactions=("${INSTALL_STATE_ROOT}"/txn.*)
  shopt -u nullglob
  for transaction in "${transactions[@]}"; do
    [[ -d "${transaction}" && ! -L "${transaction}" ]] || {
      printf 'Refusing unsafe installer transaction path: %s\n' "${transaction}" >&2
      return 1
    }
    if ! recover_transaction "${transaction}"; then
      printf 'Could not recover installer transaction: %s\n' "${transaction}" >&2
      return 1
    fi
  done
  BINARY_DESTINATION="${requested_binary_destination}"
  SKILL_DESTINATION="${requested_skill_destination}"
  BINARY_BACKUP="${requested_binary_backup}"
  SKILL_BACKUP="${requested_skill_backup}"
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

validate_destination "Installer state" "${INSTALL_STATE_ROOT}"
if [[ "${INSTALL_STATE_ROOT}" == */../* || "${INSTALL_STATE_ROOT}" == */.. ]]; then
  printf 'Installer state path must not contain parent traversal\n' >&2
  exit 1
fi
INSTALL_PATHS=("${BINARY_DESTINATION}" "${SKILL_DESTINATION}" "${BINARY_BACKUP}" "${SKILL_BACKUP}" "${INSTALL_STATE_ROOT}")
for ((LEFT_INDEX = 0; LEFT_INDEX < ${#INSTALL_PATHS[@]}; LEFT_INDEX++)); do
  for ((RIGHT_INDEX = LEFT_INDEX + 1; RIGHT_INDEX < ${#INSTALL_PATHS[@]}; RIGHT_INDEX++)); do
    if paths_overlap "${INSTALL_PATHS[LEFT_INDEX]}" "${INSTALL_PATHS[RIGHT_INDEX]}"; then
      printf 'Binary, skill, and backup paths must not overlap\n' >&2
      exit 1
    fi
  done
done

if ! ensure_directory_chain "${INSTALL_STATE_ROOT}"; then
  exit 1
fi
if ! recover_transactions; then
  exit 1
fi
BINARY_HAD_DESTINATION=0
SKILL_HAD_DESTINATION=0
TRANSACTION_STATE=""

if [[ ! -f "${SOURCE_BINARY}" || -L "${SOURCE_BINARY}" || ! -x "${SOURCE_BINARY}" ]]; then
  printf 'Release binary is missing or not executable: %s\n' "${SOURCE_BINARY}" >&2
  exit 1
fi
if ! SOURCE_VERSION_OUTPUT="$("${SOURCE_BINARY}" version)" ||
  [[ ! "${SOURCE_VERSION_OUTPUT}" =~ ^mailcli\ [0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Release binary version output is invalid\n' >&2
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
if path_present "${BINARY_BACKUP}" || path_present "${SKILL_BACKUP}"; then
  printf 'Refusing install because a backup path already exists\n' >&2
  exit 1
fi

BINARY_PARENT="$(dirname "${BINARY_DESTINATION}")"
SKILL_PARENT="$(dirname "${SKILL_DESTINATION}")"
if ! ensure_directory_chain "${BINARY_PARENT}" || ! ensure_directory_chain "${SKILL_PARENT}"; then
  exit 1
fi
if path_present "${BINARY_DESTINATION}"; then
  [[ -f "${BINARY_DESTINATION}" && ! -L "${BINARY_DESTINATION}" ]] || {
    printf 'Binary destination must be a regular file: %s\n' "${BINARY_DESTINATION}" >&2
    exit 1
  }
  BINARY_HAD_DESTINATION=1
fi
if path_present "${SKILL_DESTINATION}"; then
  [[ -d "${SKILL_DESTINATION}" && ! -L "${SKILL_DESTINATION}" ]] || {
    printf 'Skill destination must be a directory: %s\n' "${SKILL_DESTINATION}" >&2
    exit 1
  }
  if find "${SKILL_DESTINATION}" -type l -print -quit | grep -q .; then
    printf 'Skill destination must not contain symbolic links: %s\n' "${SKILL_DESTINATION}" >&2
    exit 1
  fi
  SKILL_HAD_DESTINATION=1
fi

verify_install_parents() {
  validate_existing_directory_chain "${BINARY_PARENT}" || return 1
  validate_existing_directory_chain "${SKILL_PARENT}" || return 1
}

TRANSACTION_DIRECTORY="$(mktemp -d "${INSTALL_STATE_ROOT}/txn.XXXXXX")"
chmod 0700 "${TRANSACTION_DIRECTORY}"
TRANSACTION_MANIFEST="${TRANSACTION_DIRECTORY}/manifest"
BINARY_SNAPSHOT="${TRANSACTION_DIRECTORY}/binary.snapshot"
SKILL_SNAPSHOT="${TRANSACTION_DIRECTORY}/skill.snapshot"
TRANSACTION_NAME="$(basename "${TRANSACTION_DIRECTORY}")"
BINARY_STAGE="${BINARY_PARENT}/.mailcli-binary-stage.${TRANSACTION_NAME}"
SKILL_STAGE="${SKILL_PARENT}/.mailcli-skill-stage.${TRANSACTION_NAME}"
INSTALL_COMPLETE=0

rollback_install() {
  local status=$?
  set +e
  if [[ "${INSTALL_COMPLETE}" -ne 1 && -n "${TRANSACTION_DIRECTORY}" &&
    -f "${TRANSACTION_MANIFEST}" ]]; then
    if [[ "${TRANSACTION_STATE}" == "committed" ]]; then
      finalize_committed_transaction ||
        printf 'Installer committed state cleanup remains pending: %s\n' "${TRANSACTION_DIRECTORY}" >&2
    else
      rollback_transaction ||
        printf 'Installer rollback remains pending: %s\n' "${TRANSACTION_DIRECTORY}" >&2
    fi
  elif [[ -n "${TRANSACTION_DIRECTORY}" && -d "${TRANSACTION_DIRECTORY}" ]]; then
    if [[ -n "${BINARY_STAGE}" && -f "${BINARY_STAGE}" && ! -L "${BINARY_STAGE}" ]]; then
      rm -f "${BINARY_STAGE}"
    fi
    if [[ -n "${SKILL_STAGE}" && -d "${SKILL_STAGE}" && ! -L "${SKILL_STAGE}" ]]; then
      rm -rf "${SKILL_STAGE}"
    fi
    rmdir "${TRANSACTION_DIRECTORY}" 2>/dev/null || true
  fi
  exit "${status}"
}
trap rollback_install EXIT

if ! manifest_write prepared; then
  printf 'Could not create installer transaction manifest\n' >&2
  exit 1
fi
if path_present "${BINARY_STAGE}" || ! (set -C; : >"${BINARY_STAGE}"); then
  printf 'Could not create exclusive binary staging path\n' >&2
  exit 1
fi
chmod 0755 "${BINARY_STAGE}"
cp "${SOURCE_BINARY}" "${BINARY_STAGE}"
chmod 0755 "${BINARY_STAGE}"
cmp -s "${SOURCE_BINARY}" "${BINARY_STAGE}"
BINARY_STAGE_IDENTITY="$(path_identity "${BINARY_STAGE}")"
if path_present "${SKILL_STAGE}" || ! mkdir -m 0700 "${SKILL_STAGE}"; then
  printf 'Could not create exclusive skill staging path\n' >&2
  exit 1
fi
cp -R "${SOURCE_SKILL}/." "${SKILL_STAGE}/"
diff -qr "${SOURCE_SKILL}" "${SKILL_STAGE}" >/dev/null
SKILL_STAGE_IDENTITY="$(path_identity "${SKILL_STAGE}")"
cp "${SOURCE_BINARY}" "${BINARY_SNAPSHOT}"
chmod 0755 "${BINARY_SNAPSHOT}"
mkdir -m 0700 "${SKILL_SNAPSHOT}"
cp -R "${SOURCE_SKILL}/." "${SKILL_SNAPSHOT}/"
cmp -s "${SOURCE_BINARY}" "${BINARY_SNAPSHOT}"
diff -qr "${SOURCE_SKILL}" "${SKILL_SNAPSHOT}" >/dev/null
manifest_write prepared

manifest_write binary_backup_pending
verify_install_parents
if [[ "${BINARY_HAD_DESTINATION}" -eq 1 ]]; then
  mv "${BINARY_DESTINATION}" "${BINARY_BACKUP}"
fi
sync_filesystem
manifest_write binary_backed_up

manifest_write skill_backup_pending
verify_install_parents
if [[ "${SKILL_HAD_DESTINATION}" -eq 1 ]]; then
  mv "${SKILL_DESTINATION}" "${SKILL_BACKUP}"
fi
sync_filesystem
manifest_write skill_backed_up

manifest_write binary_install_pending
verify_install_parents
component_stage_matches_snapshot binary
mv "${BINARY_STAGE}" "${BINARY_DESTINATION}"
sync_filesystem
component_matches_snapshot binary
manifest_write binary_installed

manifest_write skill_install_pending
verify_install_parents
component_stage_matches_snapshot skill
mv "${SKILL_STAGE}" "${SKILL_DESTINATION}"
sync_filesystem
component_matches_snapshot skill
manifest_write skill_installed

cmp -s "${SOURCE_BINARY}" "${BINARY_DESTINATION}"
diff -qr "${SOURCE_SKILL}" "${SKILL_DESTINATION}" >/dev/null
[[ "$("${BINARY_DESTINATION}" version)" == "${SOURCE_VERSION_OUTPUT}" ]]

manifest_write verified
manifest_write committed
finalize_committed_transaction
INSTALL_COMPLETE=1

printf 'Installed MailCLI binary at %s\n' "${BINARY_DESTINATION}"
printf 'Installed MailCLI skill at %s\n' "${SKILL_DESTINATION}"
printf 'Start a new agent session to load the installed skill.\n'

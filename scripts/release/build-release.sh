#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ $# -lt 1 ]]; then
  printf 'Usage: %s MAJOR.MINOR.PATCH\n' "$(basename "${BASH_SOURCE[0]}")" >&2
  exit 1
fi
VERSION="${1}"
RELEASE_DIRECTORY="${MAILCLI_RELEASE_DIRECTORY:-${MAILCLI_ROOT}/dist}"
ARCHIVE_ROOT="mailcli_${VERSION}_darwin_arm64"
ARCHIVE_NAME="${ARCHIVE_ROOT}.tar.gz"
ARCHIVE_PATH="${RELEASE_DIRECTORY}/${ARCHIVE_NAME}"
CHECKSUM_PATH="${RELEASE_DIRECTORY}/SHA256SUMS"
SIGNATURE_PATH="${RELEASE_DIRECTORY}/SHA256SUMS.sig"
SIGNING_KEY="${MAILCLI_RELEASE_SIGNING_KEY:-${HOME}/Library/Application Support/MailCLI/release-signing-key}"
STAGING_ROOT="${RELEASE_DIRECTORY}/.mailcli-release-staging-${VERSION}"
STAGING_ARCHIVE="${STAGING_ROOT}/${ARCHIVE_NAME}"
STAGING_CHECKSUM="${STAGING_ROOT}/SHA256SUMS"
STAGING_SIGNATURE="${STAGING_ROOT}/SHA256SUMS.sig"
STAGING_MANIFEST="${STAGING_ROOT}/STAGING-MANIFEST"
STAGING_MANIFEST_SIGNATURE="${STAGING_ROOT}/STAGING-MANIFEST.sig"
STAGING_CREATED=0
STAGING_READY=0
STAGED_FILES=("${STAGING_ARCHIVE}" "${STAGING_CHECKSUM}" "${STAGING_SIGNATURE}")
FINAL_FILES=("${ARCHIVE_PATH}" "${CHECKSUM_PATH}" "${SIGNATURE_PATH}")

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Release version must use MAJOR.MINOR.PATCH: %s\n' "${VERSION}" >&2
  exit 1
fi
if [[ "${RELEASE_DIRECTORY}" != /* ]]; then
  printf 'Release directory must be absolute: %s\n' "${RELEASE_DIRECTORY}" >&2
  exit 1
fi
path_exists() {
  [[ -e "$1" || -L "$1" ]]
}

validate_staging_manifest() {
  awk -v archive_name="${ARCHIVE_NAME}" '
    BEGIN { valid = 1 }
    function valid_digest(value) {
      return length(value) == 64 && value ~ /^[0-9a-f]+$/
    }
    {
      expected_name = NR == 1 ? archive_name : NR == 2 ? "SHA256SUMS" : NR == 3 ? "SHA256SUMS.sig" : ""
      if (NF != 2 || !valid_digest($1) || $2 != expected_name) {
        valid = 0
      }
    }
    END {
      if (NR != 3 || !valid) {
        exit 1
      }
    }
  ' "${STAGING_MANIFEST}"
}

validate_checksum_manifest() {
  awk -v archive_name="${ARCHIVE_NAME}" '
    function valid_digest(value) {
      return length(value) == 64 && value ~ /^[0-9a-f]+$/
    }
    NR == 1 && NF == 2 && valid_digest($1) && $2 == archive_name { valid = 1; next }
    { valid = 0 }
    END {
      if (NR != 1 || !valid) {
        exit 1
      }
    }
  ' "${STAGING_CHECKSUM}"
}

validate_staging_entries() {
  local ENTRY
  local ENTRY_NAME
  local ASSET_COUNT=0
  while IFS= read -r -d '' ENTRY; do
    ENTRY_NAME="${ENTRY##*/}"
    case "${ENTRY_NAME}" in
      "${ARCHIVE_NAME}" | "SHA256SUMS" | "SHA256SUMS.sig" | "STAGING-MANIFEST" | "STAGING-MANIFEST.sig")
        ASSET_COUNT=$((ASSET_COUNT + 1))
        ;;
      .publish.*)
        ;;
      *)
        printf 'Unexpected path in release staging directory: %s\n' "${ENTRY}" >&2
        return 1
        ;;
    esac
  done < <(find "${STAGING_ROOT}" -mindepth 1 -maxdepth 1 -print0)
  if [[ "${ASSET_COUNT}" -ne 5 ]]; then
    printf 'Release staging contains an unexpected asset set: %s\n' "${STAGING_ROOT}" >&2
    return 1
  fi
}

verify_signature() {
  local INPUT_PATH="$1"
  local SIGNATURE_FILE="$2"
  local VERIFY_ARGUMENTS=(verify --input "${INPUT_PATH}" --signature "${SIGNATURE_FILE}")
  if [[ -n "${MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY:-}" ]]; then
    VERIFY_ARGUMENTS+=(--public "${MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY}")
  fi
  go run -mod=readonly "${MAILCLI_ROOT}/cmd/mailcli-release-sign" "${VERIFY_ARGUMENTS[@]}"
}

verify_staged_assets() {
  local STAGED_FILE
  for STAGED_FILE in \
    "${STAGING_ARCHIVE}" "${STAGING_CHECKSUM}" "${STAGING_SIGNATURE}" \
    "${STAGING_MANIFEST}" "${STAGING_MANIFEST_SIGNATURE}"; do
    if [[ ! -f "${STAGED_FILE}" || -L "${STAGED_FILE}" ]]; then
      printf 'Release staging is missing a regular asset: %s\n' "${STAGED_FILE}" >&2
      return 1
    fi
  done
  if ! validate_staging_entries; then
    return 1
  fi
  if ! validate_staging_manifest; then
    printf 'Release staging manifest has unexpected contents: %s\n' "${STAGING_MANIFEST}" >&2
    return 1
  fi
  if ! verify_signature "${STAGING_MANIFEST}" "${STAGING_MANIFEST_SIGNATURE}"; then
    printf 'Release staging manifest signature verification failed: %s\n' "${STAGING_MANIFEST}" >&2
    return 1
  fi
  if ! (cd "${STAGING_ROOT}" && shasum -a 256 -c "$(basename "${STAGING_MANIFEST}")"); then
    printf 'Release staging manifest does not match staged assets: %s\n' "${STAGING_ROOT}" >&2
    return 1
  fi
  if ! validate_checksum_manifest; then
    printf 'Release checksum manifest has unexpected contents: %s\n' "${STAGING_CHECKSUM}" >&2
    return 1
  fi
  if ! verify_signature "${STAGING_CHECKSUM}" "${STAGING_SIGNATURE}"; then
    printf 'Release checksum signature verification failed: %s\n' "${STAGING_SIGNATURE}" >&2
    return 1
  fi
  if ! (cd "${STAGING_ROOT}" && shasum -a 256 -c "$(basename "${STAGING_CHECKSUM}")"); then
    printf 'Release archive checksum verification failed: %s\n' "${STAGING_ARCHIVE}" >&2
    return 1
  fi
}

cleanup_stale_publication_files() {
  local CANDIDATE
  local STAGED_FILE
  local MATCHED
  while IFS= read -r -d '' CANDIDATE; do
    if [[ ! -f "${CANDIDATE}" || -L "${CANDIDATE}" ]]; then
      printf 'Refusing unexpected publication temporary: %s\n' "${CANDIDATE}" >&2
      return 1
    fi
    MATCHED=0
    for STAGED_FILE in "${STAGED_FILES[@]}"; do
      if cmp -s "${STAGED_FILE}" "${CANDIDATE}"; then
        MATCHED=1
        break
      fi
    done
    if [[ "${MATCHED}" -ne 1 ]]; then
      printf 'Refusing divergent publication temporary: %s\n' "${CANDIDATE}" >&2
      return 1
    fi
    if ! rm -f "${CANDIDATE}"; then
      printf 'Could not remove verified publication temporary: %s\n' "${CANDIDATE}" >&2
      return 1
    fi
  done < <(find "${STAGING_ROOT}" -mindepth 1 -maxdepth 1 -name '.publish.*' -print0)
}

publish_asset() {
  local INDEX="$1"
  local PUBLISH_TEMP
  PUBLISH_TEMP="$(mktemp "${STAGING_ROOT}/.publish.XXXXXX")" || {
    printf 'Could not allocate publication staging space for: %s\n' "${FINAL_FILES[INDEX]}" >&2
    return 1
  }
  if ! cp "${STAGED_FILES[INDEX]}" "${PUBLISH_TEMP}" ||
    ! cmp -s "${STAGED_FILES[INDEX]}" "${PUBLISH_TEMP}" ||
    [[ "${STAGED_FILES[INDEX]}" -ef "${PUBLISH_TEMP}" ]] ||
    ! chmod 0644 "${PUBLISH_TEMP}"; then
    rm -f "${PUBLISH_TEMP}"
    printf 'Could not prepare an independent verified publication copy: %s\n' "${FINAL_FILES[INDEX]}" >&2
    return 1
  fi
  if ! link "${PUBLISH_TEMP}" "${FINAL_FILES[INDEX]}"; then
    if [[ -f "${FINAL_FILES[INDEX]}" && ! -L "${FINAL_FILES[INDEX]}" ]] &&
      cmp -s "${STAGED_FILES[INDEX]}" "${FINAL_FILES[INDEX]}"; then
      rm -f "${PUBLISH_TEMP}"
      return 0
    fi
    rm -f "${PUBLISH_TEMP}"
    printf 'Could not publish release asset without overwrite: %s\n' "${FINAL_FILES[INDEX]}" >&2
    return 1
  fi
  if ! rm -f "${PUBLISH_TEMP}"; then
    printf 'Published release asset but could not remove its private staging link: %s\n' "${FINAL_FILES[INDEX]}" >&2
    return 1
  fi
}

verify_private_staging_directory() {
  local OWNER
  local MODE
  if [[ -L "${STAGING_ROOT}" || ! -d "${STAGING_ROOT}" ]]; then
    printf 'Refusing non-directory release staging path: %s\n' "${STAGING_ROOT}" >&2
    return 1
  fi
  OWNER="$(stat -f '%u' "${STAGING_ROOT}")" || return 1
  MODE="$(stat -f '%Lp' "${STAGING_ROOT}")" || return 1
  if [[ "${OWNER}" != "$(id -u)" || ( "${MODE}" != "700" && "${MODE}" != "0700" ) ]]; then
    printf 'Refusing release staging path without owner-only permissions: %s\n' "${STAGING_ROOT}" >&2
    return 1
  fi
}

report_final_state() {
  local INDEX
  for INDEX in "${!FINAL_FILES[@]}"; do
    if path_exists "${FINAL_FILES[INDEX]}"; then
      if [[ -f "${FINAL_FILES[INDEX]}" && ! -L "${FINAL_FILES[INDEX]}" ]] &&
        cmp -s "${STAGED_FILES[INDEX]}" "${FINAL_FILES[INDEX]}"; then
        printf 'matching_final=%s\n' "${FINAL_FILES[INDEX]}"
      else
        printf 'divergent_final=%s\n' "${FINAL_FILES[INDEX]}"
      fi
    else
      printf 'missing_final=%s\n' "${FINAL_FILES[INDEX]}"
    fi
  done
}

cleanup_staging() {
  local EXIT_STATUS=$?
  trap - EXIT
  if [[ "${EXIT_STATUS}" -ne 0 ]]; then
    if [[ "${STAGING_READY}" -eq 1 ]]; then
      printf 'Release publication is incomplete; authenticated staging retained at %s\n' "${STAGING_ROOT}" >&2
      report_final_state >&2
    elif [[ "${STAGING_CREATED}" -eq 1 ]]; then
      if ! rm -rf "${STAGING_ROOT}"; then
        printf 'Could not remove failed private staging directory: %s\n' "${STAGING_ROOT}" >&2
      fi
      printf 'Release staging failed; no new final assets were published in %s\n' "${RELEASE_DIRECTORY}" >&2
    fi
  fi
  exit "${EXIT_STATUS}"
}
trap cleanup_staging EXIT

if [[ -e "${STAGING_ROOT}" || -L "${STAGING_ROOT}" ]]; then
  if ! verify_private_staging_directory || ! verify_staged_assets; then
    printf 'Refusing to publish from unauthenticated release staging; no new final assets were published: %s\n' \
      "${STAGING_ROOT}" >&2
    exit 1
  fi
else
  "${MAILCLI_ROOT}/scripts/build/build.sh"
  BINARY="${MAILCLI_BUILD_OUTPUT:-${MAILCLI_ROOT}/bin/mailcli}"
  if [[ "$("${BINARY}" version)" != "mailcli ${VERSION}" ]]; then
    printf 'Binary version does not match requested release %s\n' "${VERSION}" >&2
    exit 1
  fi
  if ! file "${BINARY}" | grep -q 'Mach-O 64-bit executable arm64'; then
    printf 'Release binary is not native darwin/arm64\n' >&2
    exit 1
  fi
  codesign --verify --strict "${BINARY}"
  if go version -m "${BINARY}" | grep -q $'\tbuild\tvcs='; then
    printf 'Release binary contains environment-dependent VCS metadata\n' >&2
    exit 1
  fi

  mkdir -p "${RELEASE_DIRECTORY}"
  if ! (umask 077 && mkdir -m 0700 "${STAGING_ROOT}"); then
    printf 'Could not create private release staging directory on the destination filesystem: %s\n' \
      "${STAGING_ROOT}" >&2
    exit 1
  fi
  STAGING_CREATED=1
  mkdir -p "${STAGING_ROOT}/${ARCHIVE_ROOT}/bin" "${STAGING_ROOT}/${ARCHIVE_ROOT}/skills"
  cp "${BINARY}" "${STAGING_ROOT}/${ARCHIVE_ROOT}/bin/mailcli"
  cp -R "${MAILCLI_ROOT}/skills/mailcli" "${STAGING_ROOT}/${ARCHIVE_ROOT}/skills/mailcli"
  cp "${MAILCLI_ROOT}/scripts/release/install.sh" "${STAGING_ROOT}/${ARCHIVE_ROOT}/install.sh"
  cp "${MAILCLI_ROOT}/README.md" "${STAGING_ROOT}/${ARCHIVE_ROOT}/README.md"
  cp "${MAILCLI_ROOT}/LICENSE" "${STAGING_ROOT}/${ARCHIVE_ROOT}/LICENSE"
  chmod 0755 "${STAGING_ROOT}/${ARCHIVE_ROOT}/bin/mailcli" "${STAGING_ROOT}/${ARCHIVE_ROOT}/install.sh"

  cmp -s "${BINARY}" "${STAGING_ROOT}/${ARCHIVE_ROOT}/bin/mailcli"
  diff -qr "${MAILCLI_ROOT}/skills/mailcli" "${STAGING_ROOT}/${ARCHIVE_ROOT}/skills/mailcli" >/dev/null
  COPYFILE_DISABLE=1 tar -czf "${STAGING_ARCHIVE}" -C "${STAGING_ROOT}" "${ARCHIVE_ROOT}"
  (
    cd "${STAGING_ROOT}"
    shasum -a 256 "${ARCHIVE_NAME}" >"$(basename "${STAGING_CHECKSUM}")"
  )
  SIGN_ARGUMENTS=(
    sign
    --private "${SIGNING_KEY}"
    --input "${STAGING_CHECKSUM}"
    --output "${STAGING_SIGNATURE}"
  )
  if [[ -n "${MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY:-}" ]]; then
    SIGN_ARGUMENTS+=(--expected-public "${MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY}")
  fi
  go run -mod=readonly "${MAILCLI_ROOT}/cmd/mailcli-release-sign" "${SIGN_ARGUMENTS[@]}"
  chmod 0644 "${STAGING_ARCHIVE}" "${STAGING_CHECKSUM}" "${STAGING_SIGNATURE}"
  (
    cd "${STAGING_ROOT}"
    shasum -a 256 "${ARCHIVE_NAME}" >"$(basename "${STAGING_MANIFEST}")"
    shasum -a 256 "$(basename "${STAGING_CHECKSUM}")" >>"$(basename "${STAGING_MANIFEST}")"
    shasum -a 256 "$(basename "${STAGING_SIGNATURE}")" >>"$(basename "${STAGING_MANIFEST}")"
  )
  STAGING_SIGN_ARGUMENTS=(
    sign
    --private "${SIGNING_KEY}"
    --input "${STAGING_MANIFEST}"
    --output "${STAGING_MANIFEST_SIGNATURE}"
  )
  if [[ -n "${MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY:-}" ]]; then
    STAGING_SIGN_ARGUMENTS+=(--expected-public "${MAILCLI_RELEASE_EXPECTED_PUBLIC_KEY}")
  fi
  go run -mod=readonly "${MAILCLI_ROOT}/cmd/mailcli-release-sign" "${STAGING_SIGN_ARGUMENTS[@]}"
  chmod 0644 "${STAGING_MANIFEST}" "${STAGING_MANIFEST_SIGNATURE}"
  rm -rf "${STAGING_ROOT:?}/${ARCHIVE_ROOT:?}"
  if ! verify_staged_assets; then
    printf 'Release asset verification failed before publication: %s\n' "${STAGING_ROOT}" >&2
    exit 1
  fi
fi
STAGING_READY=1
if ! cleanup_stale_publication_files; then
  exit 1
fi

for INDEX in "${!FINAL_FILES[@]}"; do
  if path_exists "${FINAL_FILES[INDEX]}" &&
    { [[ ! -f "${FINAL_FILES[INDEX]}" || -L "${FINAL_FILES[INDEX]}" ]] ||
      ! cmp -s "${STAGED_FILES[INDEX]}" "${FINAL_FILES[INDEX]}"; }; then
    printf 'Refusing divergent existing release asset: %s\n' "${FINAL_FILES[INDEX]}" >&2
    exit 1
  fi
done

for INDEX in "${!FINAL_FILES[@]}"; do
  if path_exists "${FINAL_FILES[INDEX]}"; then
    continue
  fi
  if ! publish_asset "${INDEX}"; then
    exit 1
  fi
done

for INDEX in "${!FINAL_FILES[@]}"; do
  if [[ ! -f "${FINAL_FILES[INDEX]}" || -L "${FINAL_FILES[INDEX]}" ]] ||
    ! cmp -s "${STAGED_FILES[INDEX]}" "${FINAL_FILES[INDEX]}"; then
    printf 'Published release asset failed byte verification: %s\n' "${FINAL_FILES[INDEX]}" >&2
    exit 1
  fi
done

if ! rm -rf "${STAGING_ROOT}"; then
  printf 'Release assets are verified but private staging could not be removed: %s\n' "${STAGING_ROOT}" >&2
  exit 1
fi
STAGING_READY=0
STAGING_CREATED=0

printf 'Built %s\n' "${ARCHIVE_PATH}"
printf 'Built %s\n' "${CHECKSUM_PATH}"
printf 'Built %s\n' "${SIGNATURE_PATH}"

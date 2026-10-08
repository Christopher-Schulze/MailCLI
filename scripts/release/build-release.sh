#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
check_go_toolchain "${MAILCLI_ROOT}"
if [[ $# -lt 1 || $# -gt 2 || ( $# -eq 2 && "$2" != --discard-stale-staging ) ]]; then
  printf 'Usage: %s MAJOR.MINOR.PATCH [--discard-stale-staging]\n' "$(basename "${BASH_SOURCE[0]}")" >&2
  exit 1
fi
VERSION="${1}"
DISCARD_STALE_STAGING="${2:-}"
SOURCE_COMMIT="$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)"
GO_VERSION="$(go env GOVERSION)"
STAGING_IDENTITY=''
STAGING_ASSET_IDENTITIES=()
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
    NR == 1 { if (NF != 2 || $1 != "source_commit" || length($2) != 40 || $2 !~ /^[0-9a-f]+$/) valid = 0; next }
    NR == 2 { if (NF != 2 || $1 != "go_version" || $2 !~ /^go[0-9]+\.[0-9]+\.[0-9]+$/) valid = 0; next }
    NR == 3 { if (NF != 2 || $1 != "binary_sha256" || !valid_digest($2)) valid = 0; next }
    {
      expected_name = NR == 4 ? archive_name : NR == 5 ? "SHA256SUMS" : NR == 6 ? "SHA256SUMS.sig" : ""
      if (NF != 2 || !valid_digest($1) || $2 != expected_name) {
        valid = 0
      }
    }
    END {
      if (NR != 6 || !valid) {
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
  STAGING_ASSET_IDENTITIES=()
  for STAGED_FILE in \
    "${STAGING_ARCHIVE}" "${STAGING_CHECKSUM}" "${STAGING_SIGNATURE}" \
    "${STAGING_MANIFEST}" "${STAGING_MANIFEST_SIGNATURE}"; do
    if [[ ! -f "${STAGED_FILE}" || -L "${STAGED_FILE}" ]]; then
      printf 'Release staging is missing a regular asset: %s\n' "${STAGED_FILE}" >&2
      return 1
    fi
    STAGING_ASSET_IDENTITIES+=("$(stat -f '%d:%i:%u:%Lp:%z:%m:%c' "${STAGED_FILE}")")
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
  if ! (cd "${STAGING_ROOT}" && awk 'NR > 3' STAGING-MANIFEST | shasum -a 256 -c -); then
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
  local BINARY_DIGEST
  BINARY_DIGEST="$(tar -xOf "${STAGING_ARCHIVE}" "${ARCHIVE_ROOT}/bin/mailcli" | shasum -a 256)" || return 1
  if [[ "${BINARY_DIGEST%% *}" != "$(awk 'NR == 3 { print $2 }' "${STAGING_MANIFEST}")" ]]; then
    printf 'Release staging binary digest does not match its signed manifest\n' >&2
    return 1
  fi
  verify_private_staging_directory || return 1
  local INDEX=0
  for STAGED_FILE in "${STAGED_FILES[@]}" "${STAGING_MANIFEST}" "${STAGING_MANIFEST_SIGNATURE}"; do
    if [[ -L "${STAGED_FILE}" || "$(stat -f '%d:%i:%u:%Lp:%z:%m:%c' "${STAGED_FILE}")" != "${STAGING_ASSET_IDENTITIES[INDEX]}" ]]; then
      printf 'Release staging asset changed during verification: %s\n' "${STAGED_FILE}" >&2
      return 1
    fi
    INDEX=$((INDEX + 1))
  done
}

cleanup_stale_publication_files() (
  local CANDIDATE
  local IDENTITY
  verify_private_staging_directory || return 1
  cd -P "${STAGING_ROOT}" || return 1
  [[ "$(stat -f '%d:%i:%u:%Lp' .)" == "${STAGING_IDENTITY}" ]] || return 1
  while IFS= read -r -d '' CANDIDATE; do
    if [[ ! -f "${CANDIDATE}" || -L "${CANDIDATE}" || "$(stat -f '%u' "${CANDIDATE}")" != "$(id -u)" ]]; then
      printf 'Refusing unexpected publication temporary: %s\n' "${CANDIDATE}" >&2
      return 1
    fi
    IDENTITY="$(stat -f '%d:%i:%u:%Lp:%z:%m:%c' "${CANDIDATE}")" || return 1
    verify_private_staging_directory || return 1
    if [[ -L "${CANDIDATE}" || "$(stat -f '%d:%i:%u:%Lp:%z:%m:%c' "${CANDIDATE}")" != "${IDENTITY}" ]]; then
      printf 'Publication temporary changed before cleanup: %s\n' "${CANDIDATE}" >&2
      return 1
    fi
    if ! rm -f "${CANDIDATE}"; then
      printf 'Could not remove verified publication temporary: %s\n' "${CANDIDATE}" >&2
      return 1
    fi
  done < <(find . -mindepth 1 -maxdepth 1 -name '.publish.*' -print0)
)

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
  local IDENTITY
  IDENTITY="$(stat -f '%d:%i:%u:%Lp' "${STAGING_ROOT}")" || return 1
  if [[ -n "${STAGING_IDENTITY}" && "${IDENTITY}" != "${STAGING_IDENTITY}" ]]; then
    printf 'Release staging directory changed identity: %s\n' "${STAGING_ROOT}" >&2
    return 1
  fi
  STAGING_IDENTITY="${IDENTITY}"
}

preflight_final_assets() {
  local MATCH_STAGING="$1"
  local INDEX
  local CONFLICTS=0
  for INDEX in "${!FINAL_FILES[@]}"; do
    if path_exists "${FINAL_FILES[INDEX]}"; then
      if [[ "${MATCH_STAGING}" == 1 && -f "${FINAL_FILES[INDEX]}" && ! -L "${FINAL_FILES[INDEX]}" ]] &&
        cmp -s "${STAGED_FILES[INDEX]}" "${FINAL_FILES[INDEX]}"; then
        continue
      fi
      printf 'Conflicting existing release asset: %s\n' "${FINAL_FILES[INDEX]}" >&2
      CONFLICTS=$((CONFLICTS + 1))
    fi
  done
  if [[ "${CONFLICTS}" -gt 0 ]]; then
    printf 'Refusing divergent existing release asset; preserve existing assets and set MAILCLI_RELEASE_DIRECTORY to a different empty absolute directory\n' >&2
    return 1
  fi
}

verify_clean_source() {
  local STAGED_COMMIT="${1:-none}"
  if [[ "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" != "${SOURCE_COMMIT}" ||
    -n "$(git -C "${MAILCLI_ROOT}" status --porcelain=v1 --untracked-files=all)" ||
    -n "$(git -C "${MAILCLI_ROOT}" ls-files -v | grep -E '^[a-zS] ' || true)" ]]; then
    printf 'Release requires unchanged clean source: staging=%s current=%s\n' "${STAGED_COMMIT}" "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" >&2
    return 1
  fi
}

discard_verified_staging() (
  local NAME
  local IDENTITY
  local INDEX=0
  cleanup_stale_publication_files || return 1
  verify_private_staging_directory || return 1
  cd -P "${STAGING_ROOT}" || return 1
  [[ "$(stat -f '%d:%i:%u:%Lp' .)" == "${STAGING_IDENTITY}" ]] || return 1
  for NAME in "${ARCHIVE_NAME}" SHA256SUMS SHA256SUMS.sig STAGING-MANIFEST STAGING-MANIFEST.sig; do
    [[ -f "${NAME}" && ! -L "${NAME}" ]] || return 1
    IDENTITY="$(stat -f '%d:%i:%u:%Lp:%z:%m:%c' "${NAME}")" || return 1
    [[ "${IDENTITY}" == "${STAGING_ASSET_IDENTITIES[INDEX]}" ]] || return 1
    verify_private_staging_directory || return 1
    [[ ! -L "${NAME}" && "$(stat -f '%d:%i:%u:%Lp:%z:%m:%c' "${NAME}")" == "${IDENTITY}" ]] || return 1
    rm -f -- "${NAME}" || return 1
    INDEX=$((INDEX + 1))
  done
  verify_private_staging_directory || return 1
  rmdir "${STAGING_ROOT}"
)

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

STAGED_COMMIT=none
if path_exists "${STAGING_ROOT}"; then
  verify_private_staging_directory || exit 1
fi
if [[ -f "${STAGING_MANIFEST}" && ! -L "${STAGING_MANIFEST}" ]]; then
  STAGED_COMMIT="$(awk 'NR == 1 { print $2 }' "${STAGING_MANIFEST}")"
fi
verify_clean_source "${STAGED_COMMIT}" || exit 1
if [[ -e "${STAGING_ROOT}" || -L "${STAGING_ROOT}" ]]; then
  if ! verify_private_staging_directory || ! verify_staged_assets; then
    printf 'Refusing to publish from unauthenticated release staging; no new final assets were published: %s\n' \
      "${STAGING_ROOT}" >&2
    exit 1
  fi
  STAGED_COMMIT="$(awk 'NR == 1 { print $2 }' "${STAGING_MANIFEST}")"
  STAGED_GO_VERSION="$(awk 'NR == 2 { print $2 }' "${STAGING_MANIFEST}")"
  if [[ "${STAGED_COMMIT}" != "${SOURCE_COMMIT}" || "${STAGED_GO_VERSION}" != "${GO_VERSION}" ]]; then
    if [[ "${DISCARD_STALE_STAGING}" != --discard-stale-staging ]]; then
      printf 'Stale release staging: staging=%s current=%s; toolchain=%s current_toolchain=%s; use --discard-stale-staging to rebuild in an empty release directory\n' \
        "${STAGED_COMMIT}" "${SOURCE_COMMIT}" "${STAGED_GO_VERSION}" "${GO_VERSION}" >&2
      exit 1
    fi
    preflight_final_assets 0 || exit 1
    discard_verified_staging || exit 1
    STAGING_IDENTITY=''
  else
    STAGING_READY=1
    preflight_final_assets 1 || exit 1
  fi
fi
if [[ "${STAGING_READY}" -eq 0 ]]; then
  preflight_final_assets 0 || exit 1
  "${MAILCLI_ROOT}/scripts/build/build.sh"
  BINARY="${MAILCLI_BUILD_OUTPUT:-${MAILCLI_ROOT}/bin/mailcli}"
  if [[ "$(MAILCLI_OUTPUT=human "${BINARY}" version)" != "mailcli ${VERSION}" ]]; then
    printf 'Binary version does not match requested release %s\n' "${VERSION}" >&2
    exit 1
  fi
  if ! file "${BINARY}" | grep -q 'Mach-O 64-bit executable arm64'; then
    printf 'Release binary is not native darwin/arm64\n' >&2
    exit 1
  fi
  codesign --verify --strict "${BINARY}"
  # A release ships the reproducible ad-hoc linker signature, never a local
  # signing identity that install-local.sh applies.
  SIGNATURE_DETAILS="$(codesign -d --verbose=2 "${BINARY}" 2>&1)"
  if ! grep -qx 'Signature=adhoc' <<<"${SIGNATURE_DETAILS}"; then
    printf 'Release binary is not ad-hoc signed\n' >&2
    exit 1
  fi
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
  verify_private_staging_directory || exit 1
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
    printf 'source_commit %s\ngo_version %s\nbinary_sha256 %s\n' \
      "${SOURCE_COMMIT}" "${GO_VERSION}" "$(shasum -a 256 "${BINARY}" | awk '{ print $1 }')" >"$(basename "${STAGING_MANIFEST}")"
    shasum -a 256 "${ARCHIVE_NAME}" >>"$(basename "${STAGING_MANIFEST}")"
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
verify_clean_source "${SOURCE_COMMIT}" || exit 1
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

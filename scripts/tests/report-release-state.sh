#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DIST_DIRECTORY="${MAILCLI_DIST_DIRECTORY:-${MAILCLI_ROOT}/dist}"
BINARY_DESTINATION="${MAILCLI_BINARY_DESTINATION:-${HOME}/.local/bin/mailcli}"
SKILL_DESTINATION="${MAILCLI_SKILL_DESTINATION:-${HOME}/.agents/skills/mailcli}"
STRICT=0
REMOTE_REQUIRED=0
MISMATCH_COUNT=0
UNAVAILABLE_COUNT=0

usage() {
  cat <<'EOF'
Usage: report-release-state.sh [--strict] [--remote-required]

Compare the current source, tag, local release assets, installed artifacts, and,
when requested, the GitHub release object. This command is read-only.

  --strict           exit 1 when any comparison is missing or mismatched
  --remote-required  require and verify the GitHub tag and release object
EOF
}

while (($# > 0)); do
  case "$1" in
    --strict)
      STRICT=1
      ;;
    --remote-required)
      REMOTE_REQUIRED=1
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Release state report requires command: %s\n' "$1" >&2
    exit 2
  fi
}

for command_name in git shasum diff wc awk grep cmp; do
  require_command "${command_name}"
done
if ((REMOTE_REQUIRED)); then
  require_command gh
  require_command jq
fi

report() {
  printf '%-24s %-12s %s\n' "$1" "$2" "$3"
}

mismatch() {
  MISMATCH_COUNT=$((MISMATCH_COUNT + 1))
  report "$1" mismatch "$2"
}

unavailable() {
  UNAVAILABLE_COUNT=$((UNAVAILABLE_COUNT + 1))
  report "$1" unavailable "$2"
}

file_sha256() {
  local path="$1"
  local digest_line
  digest_line="$(shasum -a 256 "${path}")"
  printf '%s\n' "${digest_line%% *}"
}

file_size() {
  local path="$1"
  local size
  size="$(wc -c <"${path}")"
  size="${size//[[:space:]]/}"
  printf '%s\n' "${size}"
}

inspect_file() {
  local label="$1"
  local path="$2"
  if [[ ! -f "${path}" ]]; then
    mismatch "${label}" "missing: ${path}"
    return
  fi
  report "${label}" present "${path} bytes=$(file_size "${path}") sha256=$(file_sha256 "${path}")"
}

SOURCE_VERSION=""
if SOURCE_VERSION="$(awk -F'"' '$1 ~ /^[[:space:]]*version[[:space:]]*=/ {print $2; exit}' \
  "${MAILCLI_ROOT}/internal/cli/run.go")" && [[ "${SOURCE_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  report source_version present "${SOURCE_VERSION}"
else
  mismatch source_version "could not read a MAJOR.MINOR.PATCH version from internal/cli/run.go"
  exit 1
fi

SOURCE_TAG="v${SOURCE_VERSION}"
HEAD_COMMIT="$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)"
report git_head present "${HEAD_COMMIT}"

if [[ -n "$(git -C "${MAILCLI_ROOT}" status --porcelain)" ]]; then
  mismatch worktree "uncommitted changes are present"
else
  report worktree clean "no uncommitted changes"
fi

LOCAL_TAG_COMMIT="$(git -C "${MAILCLI_ROOT}" rev-parse --verify "refs/tags/${SOURCE_TAG}^{commit}" 2>/dev/null || true)"
if [[ -z "${LOCAL_TAG_COMMIT}" ]]; then
  mismatch local_tag "${SOURCE_TAG} is missing"
elif [[ "${LOCAL_TAG_COMMIT}" == "${HEAD_COMMIT}" ]]; then
  report local_tag match "${SOURCE_TAG} -> ${LOCAL_TAG_COMMIT}"
else
  mismatch local_tag "${SOURCE_TAG} -> ${LOCAL_TAG_COMMIT}; HEAD -> ${HEAD_COMMIT}"
fi

ARCHIVE_NAME="mailcli_${SOURCE_VERSION}_darwin_arm64.tar.gz"
CHECKSUMS_NAME="SHA256SUMS"
SIGNATURE_NAME="SHA256SUMS.sig"
ARCHIVE_PATH="${DIST_DIRECTORY}/${ARCHIVE_NAME}"
CHECKSUMS_PATH="${DIST_DIRECTORY}/${CHECKSUMS_NAME}"
SIGNATURE_PATH="${DIST_DIRECTORY}/${SIGNATURE_NAME}"
inspect_file dist_archive "${ARCHIVE_PATH}"
inspect_file dist_checksums "${CHECKSUMS_PATH}"
inspect_file dist_signature "${SIGNATURE_PATH}"

if [[ -f "${CHECKSUMS_PATH}" ]]; then
  CHECKSUM_OUTPUT=""
  if CHECKSUM_OUTPUT="$(cd "${DIST_DIRECTORY}" && shasum -a 256 -c "${CHECKSUMS_NAME}" 2>&1)"; then
    report dist_checksum_check pass "${CHECKSUM_OUTPUT//$'\n'/; }"
  else
    mismatch dist_checksum_check "${CHECKSUM_OUTPUT//$'\n'/; }"
  fi
fi

if [[ -f "${ARCHIVE_PATH}" && -f "${CHECKSUMS_PATH}" ]]; then
  ARCHIVE_DIGEST="$(file_sha256 "${ARCHIVE_PATH}")"
  EXPECTED_CHECKSUM_LINE="${ARCHIVE_DIGEST}  ${ARCHIVE_NAME}"
  if grep -Fxq "${EXPECTED_CHECKSUM_LINE}" "${CHECKSUMS_PATH}"; then
    report dist_manifest_entry match "${ARCHIVE_NAME}"
  else
    mismatch dist_manifest_entry "${ARCHIVE_NAME} does not have its current SHA-256 in ${CHECKSUMS_NAME}"
  fi
fi

CHECKOUT_BINARY="${MAILCLI_ROOT}/bin/mailcli"
inspect_file checkout_binary "${CHECKOUT_BINARY}"
if [[ -f "${CHECKOUT_BINARY}" ]]; then
  if [[ ! -x "${CHECKOUT_BINARY}" ]]; then
    mismatch checkout_binary_mode "${CHECKOUT_BINARY} is not executable"
  else
    report checkout_binary_mode executable "${CHECKOUT_BINARY}"
  fi
  CHECKOUT_VERSION=""
  if CHECKOUT_VERSION="$("${CHECKOUT_BINARY}" version 2>/dev/null)"; then
    if [[ "${CHECKOUT_VERSION}" == "mailcli ${SOURCE_VERSION}" ]]; then
      report checkout_binary_version match "${CHECKOUT_VERSION}"
    else
      mismatch checkout_binary_version "${CHECKOUT_VERSION}; expected mailcli ${SOURCE_VERSION}"
    fi
  else
    mismatch checkout_binary_version "version command failed: ${CHECKOUT_BINARY}"
  fi
fi

inspect_file installed_binary "${BINARY_DESTINATION}"
if [[ -f "${BINARY_DESTINATION}" ]]; then
  if [[ ! -x "${BINARY_DESTINATION}" ]]; then
    mismatch installed_binary_mode "${BINARY_DESTINATION} is not executable"
  else
    report installed_binary_mode executable "${BINARY_DESTINATION}"
  fi
  INSTALLED_VERSION=""
  if INSTALLED_VERSION="$("${BINARY_DESTINATION}" version 2>/dev/null)"; then
    if [[ "${INSTALLED_VERSION}" == "mailcli ${SOURCE_VERSION}" ]]; then
      report installed_binary_version match "${INSTALLED_VERSION}"
    else
      mismatch installed_binary_version "${INSTALLED_VERSION}; expected mailcli ${SOURCE_VERSION}"
    fi
  else
    mismatch installed_binary_version "version command failed: ${BINARY_DESTINATION}"
  fi
fi

if [[ -f "${CHECKOUT_BINARY}" && -f "${BINARY_DESTINATION}" ]]; then
  if cmp -s "${CHECKOUT_BINARY}" "${BINARY_DESTINATION}"; then
    report installed_binary match "byte-identical to ${CHECKOUT_BINARY}"
  else
    mismatch installed_binary "not byte-identical to ${CHECKOUT_BINARY}"
  fi
fi

if [[ ! -d "${MAILCLI_ROOT}/skills/mailcli" ]]; then
  mismatch repository_skill "missing repository skill"
elif [[ ! -d "${SKILL_DESTINATION}" ]]; then
  mismatch installed_skill "missing: ${SKILL_DESTINATION}"
elif diff -qr "${MAILCLI_ROOT}/skills/mailcli" "${SKILL_DESTINATION}" >/dev/null; then
  report installed_skill match "byte-tree identical to repository skill"
else
  mismatch installed_skill "drifted from ${MAILCLI_ROOT}/skills/mailcli"
fi

REMOTE_REPO=""
REMOTE_URL="$(git -C "${MAILCLI_ROOT}" config --get remote.origin.url || true)"
case "${REMOTE_URL}" in
  https://github.com/*.git)
    REMOTE_REPO="${REMOTE_URL#https://github.com/}"
    REMOTE_REPO="${REMOTE_REPO%.git}"
    ;;
  https://github.com/*)
    REMOTE_REPO="${REMOTE_URL#https://github.com/}"
    ;;
  git@github.com:*.git)
    REMOTE_REPO="${REMOTE_URL#git@github.com:}"
    REMOTE_REPO="${REMOTE_REPO%.git}"
    ;;
  git@github.com:*)
    REMOTE_REPO="${REMOTE_URL#git@github.com:}"
    ;;
esac

if ((REMOTE_REQUIRED)); then
  if [[ -z "${REMOTE_REPO}" ]]; then
    mismatch remote_repository "origin is not a GitHub repository: ${REMOTE_URL:-missing}"
  else
    REMOTE_TAG_LINES=""
    if REMOTE_TAG_LINES="$(git -C "${MAILCLI_ROOT}" ls-remote --tags origin \
      "refs/tags/${SOURCE_TAG}" "refs/tags/${SOURCE_TAG}^{}" 2>/dev/null)"; then
      REMOTE_TAG_COMMIT="$(awk -v peeled="refs/tags/${SOURCE_TAG}^{}" -v direct="refs/tags/${SOURCE_TAG}" \
        '$2 == peeled {peeled_commit=$1} $2 == direct {direct_commit=$1} END {if (peeled_commit != "") print peeled_commit; else if (direct_commit != "") print direct_commit}' \
        <<<"${REMOTE_TAG_LINES}")"
      if [[ -z "${REMOTE_TAG_COMMIT}" ]]; then
        mismatch remote_tag "${SOURCE_TAG} is missing from origin"
      elif [[ "${REMOTE_TAG_COMMIT}" == "${HEAD_COMMIT}" ]]; then
        report remote_tag match "${SOURCE_TAG} -> ${REMOTE_TAG_COMMIT}"
      else
        mismatch remote_tag "${SOURCE_TAG} -> ${REMOTE_TAG_COMMIT}; HEAD -> ${HEAD_COMMIT}"
      fi
    else
      unavailable remote_tag "git ls-remote failed for origin"
    fi

    REMOTE_RELEASE_JSON=""
    if ! REMOTE_RELEASE_JSON="$(gh release view "${SOURCE_TAG}" --repo "${REMOTE_REPO}" \
      --json tagName,name,isDraft,isPrerelease,publishedAt,assets,url 2>/dev/null)"; then
      unavailable remote_release "gh could not read ${REMOTE_REPO} release ${SOURCE_TAG}"
    else
      REMOTE_RELEASE_TAG="$(jq -r '.tagName // empty' <<<"${REMOTE_RELEASE_JSON}")"
      REMOTE_RELEASE_NAME="$(jq -r '.name // empty' <<<"${REMOTE_RELEASE_JSON}")"
      REMOTE_RELEASE_DRAFT="$(jq -r 'if .isDraft == null then true else .isDraft end' <<<"${REMOTE_RELEASE_JSON}")"
      REMOTE_RELEASE_PRERELEASE="$(jq -r 'if .isPrerelease == null then true else .isPrerelease end' <<<"${REMOTE_RELEASE_JSON}")"
      if [[ "${REMOTE_RELEASE_TAG}" != "${SOURCE_TAG}" ]]; then
        mismatch remote_release_tag "${REMOTE_RELEASE_TAG:-missing}; expected ${SOURCE_TAG}"
      else
        report remote_release_tag match "${REMOTE_RELEASE_TAG}"
      fi
      if [[ "${REMOTE_RELEASE_NAME}" != "MailCLI ${SOURCE_TAG}" ]]; then
        mismatch remote_release_name "${REMOTE_RELEASE_NAME:-missing}; expected MailCLI ${SOURCE_TAG}"
      else
        report remote_release_name match "${REMOTE_RELEASE_NAME}"
      fi
      if [[ "${REMOTE_RELEASE_DRAFT}" == true || "${REMOTE_RELEASE_PRERELEASE}" == true ]]; then
        mismatch remote_release_state "draft=${REMOTE_RELEASE_DRAFT} prerelease=${REMOTE_RELEASE_PRERELEASE}"
      else
        report remote_release_state published "draft=false prerelease=false"
      fi

      for asset_name in "${ARCHIVE_NAME}" "${CHECKSUMS_NAME}" "${SIGNATURE_NAME}"; do
        REMOTE_ASSET_DIGEST="$(jq -r --arg name "${asset_name}" \
          '[.assets[] | select(.name == $name) | (.digest // "")][0] // empty' \
          <<<"${REMOTE_RELEASE_JSON}")"
        REMOTE_ASSET_SIZE="$(jq -r --arg name "${asset_name}" \
          '[.assets[] | select(.name == $name) | (.size // 0)][0] // 0' \
          <<<"${REMOTE_RELEASE_JSON}")"
        if [[ -z "${REMOTE_ASSET_DIGEST}" ]]; then
          mismatch "remote_asset_${asset_name}" "missing from ${SOURCE_TAG}"
          continue
        fi
        report "remote_asset_${asset_name}" present "${REMOTE_ASSET_DIGEST} bytes=${REMOTE_ASSET_SIZE}"
        LOCAL_ASSET_PATH="${DIST_DIRECTORY}/${asset_name}"
        if [[ -f "${LOCAL_ASSET_PATH}" ]]; then
          LOCAL_ASSET_DIGEST="sha256:$(file_sha256 "${LOCAL_ASSET_PATH}")"
          if [[ "${LOCAL_ASSET_DIGEST}" == "${REMOTE_ASSET_DIGEST}" ]]; then
            report "remote_asset_${asset_name}" match "local dist bytes and digest"
          else
            mismatch "remote_asset_${asset_name}" "local=${LOCAL_ASSET_DIGEST}; remote=${REMOTE_ASSET_DIGEST}"
          fi
        else
          unavailable "remote_asset_${asset_name}" "local comparison unavailable: ${LOCAL_ASSET_PATH}"
        fi
      done
    fi
  fi
else
  report remote_release skipped "pass --remote-required for origin tag and GitHub release comparison"
fi

printf '\nRelease state summary: mismatches=%d unavailable=%d\n' \
  "${MISMATCH_COUNT}" "${UNAVAILABLE_COUNT}"
if ((STRICT && (MISMATCH_COUNT > 0 || UNAVAILABLE_COUNT > 0))); then
  exit 1
fi

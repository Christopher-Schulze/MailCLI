#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  printf 'MailCLI releases require macOS on Apple silicon\n' >&2
  exit 1
fi

LATEST_PAGE="$(curl --disable --head --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time 30 --max-redirs 10 --output /dev/null --write-out '%{url_effective}' 'https://github.com/Christopher-Schulze/MailCLI/releases/latest')"
if [[ ! "${LATEST_PAGE}" =~ ^https://github[.]com/Christopher-Schulze/MailCLI/releases/tag/v([0-9]+[.][0-9]+[.][0-9]+)$ ]]; then
  printf 'Latest release did not resolve to an exact MailCLI version: %s\n' "${LATEST_PAGE}" >&2
  exit 1
fi
VERSION="${BASH_REMATCH[1]}"
ARCHIVE_NAME="mailcli_${VERSION}_darwin_arm64.tar.gz"
ARCHIVE_ROOT="mailcli_${VERSION}_darwin_arm64"
RELEASE_BASE="https://github.com/Christopher-Schulze/MailCLI/releases/download/v${VERSION}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-bootstrap.XXXXXX")"
trap 'rm -rf -- "${WORK_DIR}"' EXIT

# First install trusts GitHub HTTPS; the installed Go updater verifies the release signature.
bounded_download() {
  local url="$1" destination="$2" maximum_bytes="$3" timeout_seconds="$4"
  # The pipe also bounds unknown-length responses on system curl before 8.4.
  if ! curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time "${timeout_seconds}" --max-redirs 10 --max-filesize "${maximum_bytes}" "${url}" |
    head -c "$((maximum_bytes + 1))" >"${destination}"; then
    printf 'Download failed or exceeds %s bytes: %s\n' "${maximum_bytes}" "${url}" >&2
    return 1
  fi
  if [[ "$(wc -c <"${destination}")" -gt "${maximum_bytes}" ]]; then
    printf 'Download exceeds %s bytes: %s\n' "${maximum_bytes}" "${url}" >&2
    return 1
  fi
}
bounded_download "${RELEASE_BASE}/SHA256SUMS" "${WORK_DIR}/SHA256SUMS" 1048576 30
bounded_download "${RELEASE_BASE}/${ARCHIVE_NAME}" "${WORK_DIR}/${ARCHIVE_NAME}" 67108864 120
ARCHIVE_DIGEST="$(awk -v archive="${ARCHIVE_NAME}" '{ file_name = $2; sub(/^\*/, "", file_name); if (NF == 2 && file_name == archive) { count++; digest = $1 } } END { if (count != 1 || digest !~ /^[[:xdigit:]]{64}$/) exit 1; print digest }' "${WORK_DIR}/SHA256SUMS")" || {
  printf 'SHA256SUMS must contain exactly one valid entry for %s\n' "${ARCHIVE_NAME}" >&2
  exit 1
}
printf '%s  %s\n' "${ARCHIVE_DIGEST}" "${ARCHIVE_NAME}" >"${WORK_DIR}/archive.SHA256SUMS"
(cd "${WORK_DIR}" && shasum -a 256 -c archive.SHA256SUMS)
if ! gzip -dc "${WORK_DIR}/${ARCHIVE_NAME}" | head -c 201326593 | LC_ALL=C tar -tvf - |
  awk -v root="${ARCHIVE_ROOT}" 'BEGIN { valid = 1; count = 0; bytes = 0 }
    { name = $9; sub(/\/$/, "", name); count++;
      if (NF != 9 || $1 !~ /^[-d]/ || $5 !~ /^[0-9]+$/ || length($5) > 9 ||
          $5 > 201326592 || count > 256 || (bytes += $5) > 201326592 ||
          name !~ /^[A-Za-z0-9_.\/-]+$/ || (name != root && index(name, root "/") != 1) ||
          name ~ /(^|\/)\.\.?($|\/)/ || name ~ /^\//) { valid = 0; exit } }
    END { exit !(valid && count > 0) }'; then
  printf 'Verified archive exceeds extraction limits or contains an unsafe path or unsupported entry type\n' >&2
  exit 1
fi
tar -xzf "${WORK_DIR}/${ARCHIVE_NAME}" -C "${WORK_DIR}"
if [[ ! -x "${WORK_DIR}/${ARCHIVE_ROOT}/install.sh" ]]; then
  printf 'Verified archive has no executable installer: %s\n' "${ARCHIVE_ROOT}/install.sh" >&2
  exit 1
fi
if [[ ! -x "${WORK_DIR}/${ARCHIVE_ROOT}/bin/mailcli" ||
  "$(MAILCLI_OUTPUT=human "${WORK_DIR}/${ARCHIVE_ROOT}/bin/mailcli" version)" != "mailcli ${VERSION}" ]]; then
  printf 'Verified archive binary does not match release version %s\n' "${VERSION}" >&2
  exit 1
fi
"${WORK_DIR}/${ARCHIVE_ROOT}/install.sh"
MAILCLI_OUTPUT=human "${MAILCLI_BINARY_DESTINATION:-${HOME}/.local/bin/mailcli}" version

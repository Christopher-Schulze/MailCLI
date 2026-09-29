#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  printf 'MailCLI releases require macOS on Apple silicon\n' >&2
  exit 1
fi

LATEST_PAGE="$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --output /dev/null --write-out '%{url_effective}' 'https://github.com/Christopher-Schulze/MailCLI/releases/latest')"
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
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 -o "${WORK_DIR}/SHA256SUMS" "${RELEASE_BASE}/SHA256SUMS"
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 -o "${WORK_DIR}/${ARCHIVE_NAME}" "${RELEASE_BASE}/${ARCHIVE_NAME}"
ARCHIVE_DIGEST="$(awk -v archive="${ARCHIVE_NAME}" '{ file_name = $2; sub(/^\*/, "", file_name); if (NF == 2 && file_name == archive) { count++; digest = $1 } } END { if (count != 1 || digest !~ /^[[:xdigit:]]{64}$/) exit 1; print digest }' "${WORK_DIR}/SHA256SUMS")" || {
  printf 'SHA256SUMS must contain exactly one valid entry for %s\n' "${ARCHIVE_NAME}" >&2
  exit 1
}
printf '%s  %s\n' "${ARCHIVE_DIGEST}" "${ARCHIVE_NAME}" >"${WORK_DIR}/archive.SHA256SUMS"
(cd "${WORK_DIR}" && shasum -a 256 -c archive.SHA256SUMS)
if ! tar -tvzf "${WORK_DIR}/${ARCHIVE_NAME}" | awk -v root="${ARCHIVE_ROOT}" 'BEGIN { valid = 1; count = 0 } { name = $NF; sub(/\/$/, "", name); if ($1 !~ /^[-d]/ || (name != root && index(name, root "/") != 1) || name ~ /(^|\/)\.\.?($|\/)/ || name ~ /^\//) valid = 0; count++ } END { exit !(valid && count > 0) }'; then
  printf 'Verified archive contains an unsafe path or unsupported entry type\n' >&2
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

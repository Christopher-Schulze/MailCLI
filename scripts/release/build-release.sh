#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${1:-1.0.5}"
RELEASE_DIRECTORY="${MAILCLI_RELEASE_DIRECTORY:-${MAILCLI_ROOT}/dist}"
ARCHIVE_ROOT="mailcli_${VERSION}_darwin_arm64"
ARCHIVE_PATH="${RELEASE_DIRECTORY}/${ARCHIVE_ROOT}.tar.gz"
CHECKSUM_PATH="${RELEASE_DIRECTORY}/SHA256SUMS"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Release version must use MAJOR.MINOR.PATCH: %s\n' "${VERSION}" >&2
  exit 1
fi
if [[ "${RELEASE_DIRECTORY}" != /* ]]; then
  printf 'Release directory must be absolute: %s\n' "${RELEASE_DIRECTORY}" >&2
  exit 1
fi
if [[ -e "${ARCHIVE_PATH}" || -e "${CHECKSUM_PATH}" ]]; then
  printf 'Refusing to overwrite existing release assets in %s\n' "${RELEASE_DIRECTORY}" >&2
  exit 1
fi

"${MAILCLI_ROOT}/scripts/build/build.sh"
BINARY="${MAILCLI_ROOT}/bin/mailcli"
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

STAGING_PARENT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-release.XXXXXX")"
STAGING_ROOT="${STAGING_PARENT}/${ARCHIVE_ROOT}"
cleanup_staging() {
  if [[ "${STAGING_PARENT}" == *"/mailcli-release."* && -d "${STAGING_PARENT}" ]]; then
    rm -rf "${STAGING_PARENT}"
  fi
}
trap cleanup_staging EXIT

mkdir -p "${STAGING_ROOT}/bin" "${STAGING_ROOT}/skills/mailcli/agents" "${RELEASE_DIRECTORY}"
cp "${BINARY}" "${STAGING_ROOT}/bin/mailcli"
cp "${MAILCLI_ROOT}/skills/mailcli/SKILL.md" "${STAGING_ROOT}/skills/mailcli/SKILL.md"
cp "${MAILCLI_ROOT}/skills/mailcli/agents/openai.yaml" "${STAGING_ROOT}/skills/mailcli/agents/openai.yaml"
cp "${MAILCLI_ROOT}/scripts/release/install.sh" "${STAGING_ROOT}/install.sh"
cp "${MAILCLI_ROOT}/README.md" "${STAGING_ROOT}/README.md"
cp "${MAILCLI_ROOT}/LICENSE" "${STAGING_ROOT}/LICENSE"
chmod 0755 "${STAGING_ROOT}/bin/mailcli" "${STAGING_ROOT}/install.sh"

cmp -s "${BINARY}" "${STAGING_ROOT}/bin/mailcli"
diff -qr "${MAILCLI_ROOT}/skills/mailcli" "${STAGING_ROOT}/skills/mailcli" >/dev/null
COPYFILE_DISABLE=1 tar -czf "${ARCHIVE_PATH}" -C "${STAGING_PARENT}" "${ARCHIVE_ROOT}"
(
  cd "${RELEASE_DIRECTORY}"
  shasum -a 256 "$(basename "${ARCHIVE_PATH}")" >"$(basename "${CHECKSUM_PATH}")"
)

printf 'Built %s\n' "${ARCHIVE_PATH}"
printf 'Built %s\n' "${CHECKSUM_PATH}"

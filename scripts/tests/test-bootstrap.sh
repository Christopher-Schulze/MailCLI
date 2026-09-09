#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Bootstrap verification requires command: %s\n' "$1" >&2
    exit 2
  fi
}

for command_name in awk base64 basename cat cp dirname grep mkdir mktemp openssl rm sed shasum tar tr wc; do
  require_command "${command_name}"
done

OPENSSL_BIN="${MAILCLI_TEST_OPENSSL_BIN:-$(command -v openssl || true)}"
if [[ -z "${OPENSSL_BIN}" || ! -x "${OPENSSL_BIN}" ]]; then
  printf 'Bootstrap verification requires an independently trusted OpenSSL 3 binary\n' >&2
  exit 2
fi
OPENSSL_VERSION="$("${OPENSSL_BIN}" version 2>/dev/null || true)"
if [[ "${OPENSSL_VERSION}" != OpenSSL\ 3.* ]]; then
  printf 'Bootstrap verification requires OpenSSL 3 with Ed25519 support: %s\n' "${OPENSSL_BIN}" >&2
  exit 2
fi
BASE64_BIN="$(command -v base64)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-bootstrap-test.XXXXXX")"
cleanup_test_root() {
  if [[ "${TEST_ROOT}" == *"/mailcli-bootstrap-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup_test_root EXIT

ARCHIVE_NAME="mailcli_9.9.9_darwin_arm64.tar.gz"
ARCHIVE_ROOT="mailcli_9.9.9_darwin_arm64"
ARCHIVE="${TEST_ROOT}/${ARCHIVE_NAME}"
MANIFEST="${TEST_ROOT}/SHA256SUMS"
SIGNATURE="${TEST_ROOT}/SHA256SUMS.sig"
PRIVATE_KEY="${TEST_ROOT}/private.pem"
PUBLIC_KEY="${TEST_ROOT}/public.pem"
PAYLOAD_ROOT="${TEST_ROOT}/${ARCHIVE_ROOT}"
MARKER="${TEST_ROOT}/installer-executed"

mkdir -p "${PAYLOAD_ROOT}"
printf '%s\n' '#!/usr/bin/env bash' "printf 'executed\\n' > '${MARKER}'" >"${PAYLOAD_ROOT}/install.sh"
chmod 0755 "${PAYLOAD_ROOT}/install.sh"
(
  cd "${TEST_ROOT}"
  COPYFILE_DISABLE=1 tar -czf "${ARCHIVE_NAME}" "${ARCHIVE_ROOT}"
)
"${OPENSSL_BIN}" genpkey -algorithm ED25519 -out "${PRIVATE_KEY}" >/dev/null 2>&1
"${OPENSSL_BIN}" pkey -in "${PRIVATE_KEY}" -pubout -out "${PUBLIC_KEY}" >/dev/null 2>&1
(
  cd "${TEST_ROOT}"
  shasum -a 256 "${ARCHIVE_NAME}" >"$(basename "${MANIFEST}")"
)

sign_manifest() {
  local manifest_path="$1"
  local signature_path="$2"
  local raw_signature="${signature_path}.raw"
  "${OPENSSL_BIN}" pkeyutl -sign -inkey "${PRIVATE_KEY}" -in "${manifest_path}" -out "${raw_signature}" >/dev/null 2>&1
  "${BASE64_BIN}" -i "${raw_signature}" -o "${signature_path}"
}

verify_manifest() {
  local public_key_path="$1"
  local manifest_path="$2"
  local signature_path="$3"
  local raw_signature="${signature_path}.raw"
  "${BASE64_BIN}" -D -i "${signature_path}" -o "${raw_signature}" >/dev/null 2>&1 || return 1
  [[ "$(wc -c <"${raw_signature}" | tr -d '[:space:]')" == 64 ]] || return 1
  "${OPENSSL_BIN}" pkeyutl -verify -pubin -inkey "${public_key_path}" -sigfile "${raw_signature}" -in "${manifest_path}" >/dev/null 2>&1
}

verify_archive() {
  local archive_path="$1"
  local manifest_path="$2"
  local archive_name
  local digest
  local check_path="${manifest_path}.archive-checksum"
  archive_name="$(basename "${archive_path}")"
  digest="$(awk -v archive="${archive_name}" '{ file_name = $2; sub(/^\*/, "", file_name); if (NF == 2 && file_name == archive) { count++; value = $1 } } END { if (count != 1 || value !~ /^[[:xdigit:]]{64}$/) exit 1; print value }' "${manifest_path}")" || return 1
  printf '%s  %s\n' "${digest}" "${archive_name}" >"${check_path}"
  (
    cd "$(dirname "${archive_path}")"
    shasum -a 256 -c "${check_path}" >/dev/null
  )
}

verify_archive_layout() {
  local archive_path="$1"
  tar -tvzf "${archive_path}" | awk -v root="${ARCHIVE_ROOT}" 'BEGIN { valid = 1; count = 0 } { name = $NF; sub(/\/$/, "", name); if ($1 !~ /^[-d]/ || (name != root && index(name, root "/") != 1) || name ~ /(^|\/)\.\.?($|\/)/ || name ~ /^\//) valid = 0; count++ } END { exit !(valid && count > 0) }'
}

attempt_bootstrap() {
  local public_key_path="$1"
  local manifest_path="$2"
  local signature_path="$3"
  local archive_path="$4"
  local extraction_root="${TEST_ROOT}/extracted-$(basename "${archive_path}" .tar.gz)"
  verify_manifest "${public_key_path}" "${manifest_path}" "${signature_path}" || return 1
  verify_archive "${archive_path}" "${manifest_path}" || return 1
  verify_archive_layout "${archive_path}" || return 1
  mkdir -p "${extraction_root}"
  tar -xzf "${archive_path}" -C "${extraction_root}"
  [[ -x "${extraction_root}/${ARCHIVE_ROOT}/install.sh" ]] || return 1
  "${extraction_root}/${ARCHIVE_ROOT}/install.sh"
}

sign_manifest "${MANIFEST}" "${SIGNATURE}"
attempt_bootstrap "${PUBLIC_KEY}" "${MANIFEST}" "${SIGNATURE}" "${ARCHIVE}"
[[ -f "${MARKER}" ]]

assert_rejected_before_execution() {
  local public_key_path="$1"
  local manifest_path="$2"
  local signature_path="$3"
  local archive_path="$4"
  rm -f "${MARKER}"
  if attempt_bootstrap "${public_key_path}" "${manifest_path}" "${signature_path}" "${archive_path}"; then
    printf 'Bootstrap accepted a tampered fixture\n' >&2
    exit 1
  fi
  [[ ! -e "${MARKER}" ]]
}

TAMPERED_MANIFEST="${TEST_ROOT}/tampered-manifest"
cp "${MANIFEST}" "${TAMPERED_MANIFEST}"
printf '# tampered\n' >>"${TAMPERED_MANIFEST}"
assert_rejected_before_execution "${PUBLIC_KEY}" "${TAMPERED_MANIFEST}" "${SIGNATURE}" "${ARCHIVE}"

TAMPERED_SIGNATURE="${TEST_ROOT}/tampered-signature"
printf 'not-a-signature\n' >"${TAMPERED_SIGNATURE}"
assert_rejected_before_execution "${PUBLIC_KEY}" "${MANIFEST}" "${TAMPERED_SIGNATURE}" "${ARCHIVE}"

OTHER_PRIVATE_KEY="${TEST_ROOT}/other-private.pem"
OTHER_PUBLIC_KEY="${TEST_ROOT}/other-public.pem"
"${OPENSSL_BIN}" genpkey -algorithm ED25519 -out "${OTHER_PRIVATE_KEY}" >/dev/null 2>&1
"${OPENSSL_BIN}" pkey -in "${OTHER_PRIVATE_KEY}" -pubout -out "${OTHER_PUBLIC_KEY}" >/dev/null 2>&1
assert_rejected_before_execution "${OTHER_PUBLIC_KEY}" "${MANIFEST}" "${SIGNATURE}" "${ARCHIVE}"

TAMPERED_ARCHIVE_DIR="${TEST_ROOT}/tampered-archive"
mkdir -p "${TAMPERED_ARCHIVE_DIR}"
TAMPERED_ARCHIVE="${TAMPERED_ARCHIVE_DIR}/${ARCHIVE_NAME}"
cp "${ARCHIVE}" "${TAMPERED_ARCHIVE}"
printf 'tampered archive\n' >>"${TAMPERED_ARCHIVE}"
assert_rejected_before_execution "${PUBLIC_KEY}" "${MANIFEST}" "${SIGNATURE}" "${TAMPERED_ARCHIVE}"

UNEXPECTED_MANIFEST="${TEST_ROOT}/unexpected-manifest"
sed "s/${ARCHIVE_NAME}/unexpected.tar.gz/" "${MANIFEST}" >"${UNEXPECTED_MANIFEST}"
UNEXPECTED_SIGNATURE="${TEST_ROOT}/unexpected-signature"
sign_manifest "${UNEXPECTED_MANIFEST}" "${UNEXPECTED_SIGNATURE}"
assert_rejected_before_execution "${PUBLIC_KEY}" "${UNEXPECTED_MANIFEST}" "${UNEXPECTED_SIGNATURE}" "${ARCHIVE}"

DUPLICATE_MANIFEST="${TEST_ROOT}/duplicate-manifest"
cp "${MANIFEST}" "${DUPLICATE_MANIFEST}"
cat "${MANIFEST}" >>"${DUPLICATE_MANIFEST}"
DUPLICATE_SIGNATURE="${TEST_ROOT}/duplicate-signature"
sign_manifest "${DUPLICATE_MANIFEST}" "${DUPLICATE_SIGNATURE}"
assert_rejected_before_execution "${PUBLIC_KEY}" "${DUPLICATE_MANIFEST}" "${DUPLICATE_SIGNATURE}" "${ARCHIVE}"

MALICIOUS_PARENT="${TEST_ROOT}/malicious-parent"
MALICIOUS_ROOT="${MALICIOUS_PARENT}/${ARCHIVE_ROOT}"
MALICIOUS_ARCHIVE_DIR="${TEST_ROOT}/malicious-archive"
mkdir -p "${MALICIOUS_ROOT}" "${MALICIOUS_ARCHIVE_DIR}"
ln -s /tmp "${MALICIOUS_ROOT}/link"
MALICIOUS_ARCHIVE="${MALICIOUS_ARCHIVE_DIR}/${ARCHIVE_NAME}"
(
  cd "${MALICIOUS_PARENT}"
  COPYFILE_DISABLE=1 tar -czf "${MALICIOUS_ARCHIVE}" "${ARCHIVE_ROOT}"
)
MALICIOUS_MANIFEST="${TEST_ROOT}/malicious-manifest"
shasum -a 256 "${MALICIOUS_ARCHIVE}" >"${MALICIOUS_MANIFEST}"
MALICIOUS_SIGNATURE="${TEST_ROOT}/malicious-signature"
sign_manifest "${MALICIOUS_MANIFEST}" "${MALICIOUS_SIGNATURE}"
assert_rejected_before_execution "${PUBLIC_KEY}" "${MALICIOUS_MANIFEST}" "${MALICIOUS_SIGNATURE}" "${MALICIOUS_ARCHIVE}"

for required_text in \
  'set -euo pipefail' \
  'VjVSufeZlmmMshZYeMB9u1xKoMvRavstpFqByv8Vzqg=' \
  'pkeyutl -verify' \
  'shasum -a 256 -c archive.SHA256SUMS' \
  'tar -tvzf' \
  'tar -xzf'; do
  if ! grep -Fq "${required_text}" "${MAILCLI_ROOT}/README.md"; then
    printf 'README bootstrap instructions are missing: %s\n' "${required_text}" >&2
    exit 1
  fi
done

printf 'Bootstrap authenticity tests passed\n'

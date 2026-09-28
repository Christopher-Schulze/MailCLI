#!/usr/bin/env bash
# Creates a local code-signing identity for source installs, once per user.
# A stable signature lets macOS keep one "Always Allow" Keychain decision
# across rebuilds; ad-hoc signed builds ask again after every build.
set -euo pipefail

SIGNING_NAME="MailCLI Local Signing"
KEYCHAIN="${MAILCLI_SIGNING_KEYCHAIN:-${HOME}/Library/Keychains/mailcli-local-signing.keychain-db}"

if [[ "$#" -ne 0 ]]; then
  printf 'Usage: %s\n' "$(basename "${BASH_SOURCE[0]}")" >&2
  exit 2
fi
if [[ -e "${KEYCHAIN}" ]]; then
  if security find-certificate -c "${SIGNING_NAME}" "${KEYCHAIN}" >/dev/null 2>&1; then
    printf 'Local signing identity already exists in %s\n' "${KEYCHAIN}"
    exit 0
  fi
  printf 'Refusing to reuse %s: it holds no %s identity\n' "${KEYCHAIN}" "${SIGNING_NAME}" >&2
  exit 1
fi

WORK_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-signing.XXXXXX")"
trap 'rm -rf -- "${WORK_ROOT}"' EXIT
chmod 0700 "${WORK_ROOT}"
cat >"${WORK_ROOT}/identity.cnf" <<EOF
[req]
distinguished_name = dn
x509_extensions = ext
prompt = no
[dn]
CN = ${SIGNING_NAME}
[ext]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
EOF
P12_PASSWORD="$(/usr/bin/openssl rand -hex 16)"
/usr/bin/openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -config "${WORK_ROOT}/identity.cnf" \
  -keyout "${WORK_ROOT}/key.pem" -out "${WORK_ROOT}/cert.pem" 2>/dev/null
/usr/bin/openssl pkcs12 -export -inkey "${WORK_ROOT}/key.pem" -in "${WORK_ROOT}/cert.pem" \
  -out "${WORK_ROOT}/identity.p12" -passout "pass:${P12_PASSWORD}"

# The keychain holds only this identity and has an empty password, so
# install-local.sh can unlock it without a prompt.
security create-keychain -p "" "${KEYCHAIN}"
security set-keychain-settings "${KEYCHAIN}"
security unlock-keychain -p "" "${KEYCHAIN}"
security import "${WORK_ROOT}/identity.p12" -k "${KEYCHAIN}" -P "${P12_PASSWORD}" -T /usr/bin/codesign >/dev/null
security set-key-partition-list -S apple-tool:,apple: -s -k "" "${KEYCHAIN}" >/dev/null
# codesign finds identities only in the user search list.
SEARCH_LIST=()
while IFS= read -r ENTRY; do
  ENTRY="${ENTRY#"${ENTRY%%[![:space:]]*}"}"
  SEARCH_LIST+=("${ENTRY//\"/}")
done < <(security list-keychains -d user)
security list-keychains -d user -s "${SEARCH_LIST[@]}" "${KEYCHAIN}"
printf 'Created %s in %s\n' "${SIGNING_NAME}" "${KEYCHAIN}"

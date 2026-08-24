#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${MAILCLI_ROOT}"

FORBIDDEN_PATHS=(
  "${MAILCLI_ROOT}/internal/search"
  "${MAILCLI_ROOT}/internal/mailapp/scripts/message_batch.applescript"
  "${MAILCLI_ROOT}/internal/mailapp/scripts/message_batch.js"
)
for FORBIDDEN_PATH in "${FORBIDDEN_PATHS[@]}"; do
  if [[ -e "${FORBIDDEN_PATH}" ]]; then
    printf 'Owned-index path must not exist: %s\n' "${FORBIDDEN_PATH}" >&2
    exit 1
  fi
done

while IFS= read -r -d '' SCRIPT_PATH; do
  if [[ ! -x "${SCRIPT_PATH}" ]]; then
    printf 'Shell script must be executable: %s\n' "${SCRIPT_PATH}" >&2
    exit 1
  fi
  bash -n "${SCRIPT_PATH}"
done < <(find "${MAILCLI_ROOT}/scripts" -type f -name '*.sh' -print0)

UNFORMATTED="$(gofmt -l .)"
if [[ -n "${UNFORMATTED}" ]]; then
  printf 'Unformatted Go files:\n%s\n' "${UNFORMATTED}" >&2
  exit 1
fi

go mod verify

STATICCHECK_BIN="$(command -v staticcheck || true)"
if [[ -z "${STATICCHECK_BIN}" ]]; then
  STATICCHECK_BIN="$(go env GOPATH)/bin/staticcheck"
fi
if [[ ! -x "${STATICCHECK_BIN}" ]]; then
  printf 'Staticcheck is required; install it or add it to PATH\n' >&2
  exit 1
fi
"${STATICCHECK_BIN}" ./...

go vet ./...
go test -count=1 -race -cover ./...
"${MAILCLI_ROOT}/scripts/tests/test-release.sh"

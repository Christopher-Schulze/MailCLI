#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${MAILCLI_ROOT}"

MAILCLI_TEST_CPUS="${MAILCLI_TEST_CPUS:-4}"
MAILCLI_TEST_PACKAGES="${MAILCLI_TEST_PACKAGES:-2}"
for CONCURRENCY_VALUE in "${MAILCLI_TEST_CPUS}" "${MAILCLI_TEST_PACKAGES}"; do
  if [[ ! "${CONCURRENCY_VALUE}" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Verification concurrency must be a positive integer: %s\n' "${CONCURRENCY_VALUE}" >&2
    exit 1
  fi
done
export GOMAXPROCS="${MAILCLI_TEST_CPUS}"
printf 'Verification concurrency: GOMAXPROCS=%s, packages=%s\n' \
  "${MAILCLI_TEST_CPUS}" "${MAILCLI_TEST_PACKAGES}"

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

go vet -p "${MAILCLI_TEST_PACKAGES}" ./...

GOLANGCI_LINT_BIN="$(command -v golangci-lint || true)"
if [[ -z "${GOLANGCI_LINT_BIN}" ]]; then
  GOLANGCI_LINT_BIN="$(go env GOPATH)/bin/golangci-lint"
fi
if [[ ! -x "${GOLANGCI_LINT_BIN}" ]]; then
  printf 'golangci-lint is required; install it or add it to PATH\n' >&2
  exit 1
fi
"${GOLANGCI_LINT_BIN}" run --concurrency "${MAILCLI_TEST_CPUS}" ./...

GOVULNCHECK_BIN="$(command -v govulncheck || true)"
if [[ -z "${GOVULNCHECK_BIN}" ]]; then
  GOVULNCHECK_BIN="$(go env GOPATH)/bin/govulncheck"
fi
if [[ ! -x "${GOVULNCHECK_BIN}" ]]; then
  printf 'govulncheck is required; install it or add it to PATH\n' >&2
  exit 1
fi
"${GOVULNCHECK_BIN}" ./...

go test -count=1 -race -cover -p "${MAILCLI_TEST_PACKAGES}" \
  -parallel "${MAILCLI_TEST_CPUS}" ./...
"${MAILCLI_ROOT}/scripts/tests/test-release.sh"

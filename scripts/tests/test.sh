#!/usr/bin/env bash
set -euo pipefail

SCRIPT_BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_ROOT="${MAILCLI_ROOT:-${SCRIPT_BASE}}"
export MAILCLI_ROOT
cd "${MAILCLI_ROOT}"
unset MAILCLI_BINARY_DESTINATION MAILCLI_SKILL_DESTINATION

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
REQUIRED_SKILL_STRINGS=(
  '## Error contract'
  'drafts inspect --ref REF --json'
  'mailcli sync --check'
  'without `--check`'
  'never ask the user to paste account passwords'
  'mailcli send setup'
  'compose_automation_unsupported'
  'data.page.coverage.complete'
  'scripts/utils/mailcli-preflight.sh capabilities'
  'binary SHA-256'
  'Never cache `doctor --live`'
)
for REQUIRED_STRING in "${REQUIRED_SKILL_STRINGS[@]}"; do
  if ! grep -Fq "${REQUIRED_STRING}" "${MAILCLI_ROOT}/skills/mailcli/SKILL.md" \
    "${MAILCLI_ROOT}/skills/mailcli/references/"*.md; then
    printf 'Agent skill is missing required contract text: %s\n' "${REQUIRED_STRING}" >&2
    exit 1
  fi
done
SKILL_BYTES="$(wc -c <"${MAILCLI_ROOT}/skills/mailcli/SKILL.md")"
SKILL_BYTES="${SKILL_BYTES//[[:space:]]/}"
if ((SKILL_BYTES > 9000)); then
  printf 'Agent skill entrypoint exceeds its 9000-byte context budget: %s bytes\n' "${SKILL_BYTES}" >&2
  exit 1
fi

"${SCRIPT_BASE}/scripts/tests/test-preflight-cache.sh"
"${SCRIPT_BASE}/scripts/tests/test-bootstrap.sh"
"${SCRIPT_BASE}/scripts/tests/test-install-local.sh"
"${SCRIPT_BASE}/scripts/tests/test-skill-drift.sh"

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

MAILCLI_LIVE_TESTS= MAILCLI_KEYCHAIN_LIVE= \
  go test -count=1 -race -cover -p "${MAILCLI_TEST_PACKAGES}" \
  -parallel "${MAILCLI_TEST_CPUS}" ./...
"${SCRIPT_BASE}/scripts/tests/test-task-history-export.sh"
"${SCRIPT_BASE}/scripts/tests/test-write-coordination.sh"
"${SCRIPT_BASE}/scripts/tests/test-task-ci-report.sh"
"${SCRIPT_BASE}/scripts/tests/test-commit-authority.sh"
"${SCRIPT_BASE}/scripts/tests/test-release-authority.sh"
RELEASE_REFS_BEFORE="$(git for-each-ref --format='%(refname) %(objectname)' \
  refs/heads refs/remotes refs/tags)"
"${SCRIPT_BASE}/scripts/tests/test-release.sh"
RELEASE_REFS_AFTER="$(git for-each-ref --format='%(refname) %(objectname)' \
  refs/heads refs/remotes refs/tags)"
if [[ "${RELEASE_REFS_AFTER}" != "${RELEASE_REFS_BEFORE}" ]]; then
  printf 'Local release verification changed branch, remote-tracking, or tag refs\n' >&2
  exit 1
fi
printf 'Local release verification preserved branch, remote-tracking, and tag refs\n'

# Opt-in live gates. Each stage prints an explicit skip line when its flag is
# unset so the default suite stays free of live prompts and live coverage is
# never silently absent from the output. The main go test run above clears
# both flags so live tests execute exactly once, inside their named stage.
if [[ "${MAILCLI_LIVE_TESTS:-}" == "1" ]]; then
  printf 'MAILCLI_LIVE_TESTS=1: running the live Mail-store gate\n'
  go test -count=1 -race -run '^TestLive' -v ./internal/mailstore
else
  printf 'Skipping live Mail-store gate (set MAILCLI_LIVE_TESTS=1 to enable)\n'
fi
if [[ "${MAILCLI_KEYCHAIN_LIVE:-}" == "1" ]]; then
  printf 'MAILCLI_KEYCHAIN_LIVE=1: running the live Keychain gate\n'
  go test -count=1 -race -run '^TestLiveKeychain$' -v ./internal/keychain
else
  printf 'Skipping live Keychain gate (set MAILCLI_KEYCHAIN_LIVE=1 to enable)\n'
fi
if [[ "${MAILCLI_LIVE_RESPONSIVENESS:-}" == "1" ]]; then
  printf 'MAILCLI_LIVE_RESPONSIVENESS=1: building bin/mailcli for the Mail responsiveness gate\n'
  go build -o "${MAILCLI_ROOT}/bin/mailcli" ./cmd/mailcli
  "${SCRIPT_BASE}/scripts/tests/test-live-responsiveness.sh"
else
  printf 'Skipping live Mail responsiveness gate (set MAILCLI_LIVE_RESPONSIVENESS=1 to enable)\n'
fi

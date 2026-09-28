#!/usr/bin/env bash
# MAILCLI_GATE_HARNESS=staged-v1
set -euo pipefail

SCRIPT_BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_ROOT="${MAILCLI_ROOT:-${SCRIPT_BASE}}"
export MAILCLI_ROOT
SHELL_TESTS=(
  scripts/tests/test-preflight-cache.sh
  scripts/tests/test-bootstrap.sh
  scripts/tests/test-benchmark-summary.sh
  scripts/tests/test-install-local.sh
  scripts/tests/test-skill-drift.sh
  scripts/tests/test-write-coordination.sh
  scripts/tests/test-commit-authority.sh
  scripts/tests/test-release-authority.sh
  scripts/tests/test-staged-gate.sh
  scripts/tests/test-verification-policy.sh
  scripts/tests/test-fast-gate.sh
  scripts/tests/test-release.sh
)
LIVE_SHELL_TESTS=(scripts/tests/test-live-responsiveness.sh)
case "${1:-}" in
  --list-shell-tests) printf '%s\n' "${SHELL_TESTS[@]}"; exit 0 ;;
  --list-live-shell-tests) printf '%s\n' "${LIVE_SHELL_TESTS[@]}"; exit 0 ;;
  --core-source | --core-only) ;;
  --full) exec "${MAILCLI_ROOT}/scripts/utils/manage-full-proof.sh" run "${MAILCLI_ROOT}" ;;
  --push-check) exec "${MAILCLI_ROOT}/scripts/utils/manage-full-proof.sh" check "${MAILCLI_ROOT}" ;;
  --full-checks) ;;
  --fast | '')
    INDEX_TREE="$(git -C "${MAILCLI_ROOT}" write-tree)"
    exec "${MAILCLI_ROOT}/scripts/utils/run-staged-gate.sh" "${MAILCLI_ROOT}" \
      "$(git -C "${MAILCLI_ROOT}" rev-parse HEAD)" "${INDEX_TREE}" --fast ;;
  --checks) [[ "$#" -ge 2 ]] || { printf 'Specify at least one registered check\n' >&2; exit 2; } ;;
  *) printf 'Unknown verification argument: %s\n' "$1" >&2; exit 2 ;;
esac

run_shell_test() {
  local RELATIVE_PATH="$1"
  local BEFORE_HASH
  local AFTER_HASH
  BEFORE_HASH="$(shasum -a 256 "${SCRIPT_BASE}/${RELATIVE_PATH}" | awk '{print $1}')"
  "${SCRIPT_BASE}/${RELATIVE_PATH}" || exit "$?"
  AFTER_HASH="$(shasum -a 256 "${SCRIPT_BASE}/${RELATIVE_PATH}" | awk '{print $1}')"
  [[ "${AFTER_HASH}" == "${BEFORE_HASH}" ]] || {
    printf 'Executed shell test changed its own source: %s\n' "${RELATIVE_PATH}" >&2
    exit 1
  }
  if [[ -n "${MAILCLI_GATE_RECEIPTS:-}" ]]; then
    printf '%s\t%s\n' "${BEFORE_HASH}" "${RELATIVE_PATH}" >>"${MAILCLI_GATE_RECEIPTS}"
  fi
}

cd "${MAILCLI_ROOT}"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
if [[ "${1:-}" == --core-source ]]; then
  check_go_toolchain "${MAILCLI_ROOT}" >/dev/null
else
  check_go_toolchain "${MAILCLI_ROOT}"
fi
unset MAILCLI_BINARY_DESTINATION MAILCLI_SKILL_DESTINATION

MAILCLI_TEST_CPUS="${MAILCLI_TEST_CPUS:-4}"
MAILCLI_TEST_PACKAGES="${MAILCLI_TEST_PACKAGES:-4}"

# Gate builds must not rewrite the ignored production binary: installer and
# release tests compile into private output so the lease's ignored-asset
# fingerprint stays stable even when the staged patch changes Go sources.
MAILCLI_GATE_BUILD_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-gate-build.XXXXXX")"
trap 'rm -rf -- "${MAILCLI_GATE_BUILD_ROOT}"' EXIT
export MAILCLI_BUILD_OUTPUT="${MAILCLI_GATE_BUILD_ROOT}/mailcli"
if [[ "${1:-}" == --checks ]]; then
  shift
  for CHECK_PATH in "$@"; do
    REGISTERED=false
    for REGISTERED_PATH in "${SHELL_TESTS[@]}"; do
      [[ "${CHECK_PATH}" != "${REGISTERED_PATH}" ]] || REGISTERED=true
    done
    [[ "${REGISTERED}" == true ]] || { printf 'Unregistered selected check: %s\n' "${CHECK_PATH}" >&2; exit 2; }
    run_shell_test "${CHECK_PATH}"
  done
  printf 'targeted_gate=passed\nfull_gate=deferred\n'
  exit 0
fi
for CONCURRENCY_VALUE in "${MAILCLI_TEST_CPUS}" "${MAILCLI_TEST_PACKAGES}"; do
  if [[ ! "${CONCURRENCY_VALUE}" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Verification concurrency must be a positive integer: %s\n' "${CONCURRENCY_VALUE}" >&2
    exit 1
  fi
done
export GOMAXPROCS="${MAILCLI_TEST_CPUS}"
if [[ "${1:-}" != --core-source ]]; then
  printf 'Verification concurrency: GOMAXPROCS=%s, packages=%s\n' \
    "${MAILCLI_TEST_CPUS}" "${MAILCLI_TEST_PACKAGES}"
fi

validate_product_contract() {
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
  if ((SKILL_BYTES > 4000)); then
    printf 'Agent skill entrypoint exceeds its 4000-byte context budget: %s bytes\n' "${SKILL_BYTES}" >&2
    exit 1
  fi
}

run_go_checks() {
  while IFS= read -r -d '' SCRIPT_PATH; do
    if [[ ! -x "${SCRIPT_PATH}" ]]; then
      printf 'Shell script must be executable: %s\n' "${SCRIPT_PATH}" >&2
      exit 1
    fi
    bash -n "${SCRIPT_PATH}" || exit "$?"
  done < <(find "${MAILCLI_ROOT}/scripts" -type f -name '*.sh' -print0)

  UNFORMATTED="$(gofmt -l .)" || exit "$?"
  if [[ -n "${UNFORMATTED}" ]]; then
    printf 'Unformatted Go files:\n%s\n' "${UNFORMATTED}" >&2
    exit 1
  fi

  go mod verify || exit "$?"

  GOLANGCI_LINT_BIN="$(command -v golangci-lint || true)"
  if [[ -z "${GOLANGCI_LINT_BIN}" ]]; then
    GOLANGCI_LINT_BIN="$(go env GOPATH)/bin/golangci-lint"
  fi
  if [[ ! -x "${GOLANGCI_LINT_BIN}" ]]; then
    printf 'golangci-lint is required; install it or add it to PATH\n' >&2
    exit 1
  fi
  "${GOLANGCI_LINT_BIN}" run --config "${MAILCLI_ROOT}/.golangci.yml" \
    --concurrency "${MAILCLI_TEST_CPUS}" ./... || exit "$?"

  "${MAILCLI_ROOT}/scripts/utils/run-vulnerability-check.sh" || exit "$?"

  MAILCLI_LIVE_TESTS= MAILCLI_KEYCHAIN_LIVE= \
    go test -vet=off -count=1 -cover -p "${MAILCLI_TEST_PACKAGES}" \
    -parallel "${MAILCLI_TEST_CPUS}" ./... || exit "$?"
}

run_core_checks() {
  set -euo pipefail
  validate_product_contract
  run_go_checks
  if [[ -n "${MAILCLI_GATE_RECEIPTS:-}" ]]; then
    CORE_HASH="$(declare -f validate_product_contract run_go_checks run_core_checks | shasum -a 256 | awk '{print $1}')"
    printf '%s\tcore-checks\n' "${CORE_HASH}" >>"${MAILCLI_GATE_RECEIPTS}"
  fi
}

if [[ "${1:-}" == --core-source ]]; then
  declare -f validate_product_contract run_go_checks run_core_checks
  exit 0
fi
if [[ "${1:-}" == --core-only ]]; then
  run_core_checks
  exit 0
fi

# Shell regressions run in one ordered lane beside the Go checks; the lane's
# output is printed once it finishes.
run_shell_lane() {
  set -euo pipefail
  run_shell_test scripts/tests/test-preflight-cache.sh
  run_shell_test scripts/tests/test-bootstrap.sh
  run_shell_test scripts/tests/test-benchmark-summary.sh
  run_shell_test scripts/tests/test-install-local.sh
  run_shell_test scripts/tests/test-skill-drift.sh
  run_shell_test scripts/tests/test-write-coordination.sh
  run_shell_test scripts/tests/test-commit-authority.sh
  run_shell_test scripts/tests/test-release-authority.sh
  run_shell_test scripts/tests/test-staged-gate.sh
  run_shell_test scripts/tests/test-verification-policy.sh
  run_shell_test scripts/tests/test-fast-gate.sh
  local REFS_BEFORE REFS_AFTER
  REFS_BEFORE="$(git for-each-ref --format='%(refname) %(objectname)' refs/heads refs/remotes refs/tags)"
  run_shell_test scripts/tests/test-release.sh
  REFS_AFTER="$(git for-each-ref --format='%(refname) %(objectname)' refs/heads refs/remotes refs/tags)"
  if [[ "${REFS_AFTER}" != "${REFS_BEFORE}" ]]; then
    printf 'Local release verification changed branch, remote-tracking, or tag refs\n' >&2
    exit 1
  fi
  printf 'Local release verification preserved branch, remote-tracking, and tag refs\n'
}
SHELL_LANE_LOG="${MAILCLI_GATE_BUILD_ROOT}/shell-lane.log"
set -m
run_shell_lane >"${SHELL_LANE_LOG}" 2>&1 &
SHELL_LANE_PID=$!
set +m
trap 'kill -TERM -- "-${SHELL_LANE_PID}" 2>/dev/null || true; wait "${SHELL_LANE_PID}" 2>/dev/null || true; rm -rf -- "${MAILCLI_GATE_BUILD_ROOT}"' EXIT
run_core_checks
SHELL_LANE_STATUS=0
wait "${SHELL_LANE_PID}" || SHELL_LANE_STATUS=$?
cat "${SHELL_LANE_LOG}"
[[ "${SHELL_LANE_STATUS}" == 0 ]] || exit "${SHELL_LANE_STATUS}"

# Opt-in live gates. Each stage prints an explicit skip line when its flag is
# unset so the default suite stays free of live prompts and live coverage is
# never silently absent from the output. The main go test run above clears
# both flags so live tests execute exactly once, inside their named stage.
if [[ "${MAILCLI_LIVE_TESTS:-}" == "1" ]]; then
  printf 'MAILCLI_LIVE_TESTS=1: running the live Mail-store gate\n'
  go test -count=1 -run '^TestLive' -v ./internal/mailstore
else
  printf 'Skipping live Mail-store gate (set MAILCLI_LIVE_TESTS=1 to enable)\n'
fi
if [[ "${MAILCLI_KEYCHAIN_LIVE:-}" == "1" ]]; then
  printf 'MAILCLI_KEYCHAIN_LIVE=1: running the live Keychain gate\n'
  go test -count=1 -run '^TestLiveKeychain$' -v ./internal/keychain
else
  printf 'Skipping live Keychain gate (set MAILCLI_KEYCHAIN_LIVE=1 to enable)\n'
fi
if [[ "${MAILCLI_LIVE_RESPONSIVENESS:-}" == "1" ]]; then
  printf 'MAILCLI_LIVE_RESPONSIVENESS=1: building an isolated binary for the Mail responsiveness gate\n'
  go build -mod=readonly -o "${MAILCLI_BUILD_OUTPUT}" ./cmd/mailcli
  MAILCLI_BINARY="${MAILCLI_BUILD_OUTPUT}" run_shell_test scripts/tests/test-live-responsiveness.sh
else
  printf 'Skipping live Mail responsiveness gate (set MAILCLI_LIVE_RESPONSIVENESS=1 to enable)\n'
fi

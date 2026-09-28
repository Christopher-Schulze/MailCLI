#!/usr/bin/env bash
set -euo pipefail

ROOT="${MAILCLI_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
source "${ROOT}/scripts/utils/check-go-toolchain.sh"
source "${ROOT}/scripts/utils/run-vulnerability-check.sh"
check_go_toolchain "${ROOT}"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-verification-policy.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
STATUS=0
GOTOOLCHAIN=auto check_go_toolchain "${ROOT}" >"${TEST_ROOT}/toolchain.log" 2>&1 || STATUS=$?
[[ "${STATUS}" == 2 ]]
grep -q 'Conflicting GOTOOLCHAIN' "${TEST_ROOT}/toolchain.log"

printf 'network is unreachable\n' >"${TEST_ROOT}/offline.log"
printf 'vulnerabilities found\n' >"${TEST_ROOT}/advisory.log"
printf 'invalid package\n' >"${TEST_ROOT}/error.log"
for CASE in '0 passed 0' '3 advisory_failed 3' '1 skipped_offline 1' '2 unavailable 2'; do
  read -r INPUT EXPECTED EXPECTED_STATUS <<<"${CASE}"
  LOG="${TEST_ROOT}/error.log"
  [[ "${INPUT}" != 1 ]] || LOG="${TEST_ROOT}/offline.log"
  [[ "${INPUT}" != 3 ]] || LOG="${TEST_ROOT}/advisory.log"
  STATUS=0
  classify_vulnerability_check "${INPUT}" "${LOG}" >"${TEST_ROOT}/result.log" 2>&1 || STATUS=$?
  [[ "${STATUS}" == "${EXPECTED_STATUS}" ]]
  grep -qx "vulnerability_check=${EXPECTED}" "${TEST_ROOT}/result.log"
done

LINTER="$(command -v golangci-lint || true)"
[[ -n "${LINTER}" ]] || LINTER="$(go env GOPATH)/bin/golangci-lint"
"${LINTER}" config verify --config "${ROOT}/.golangci.yml"
printf 'module verificationfixture\n\ngo %s\n' "${GOTOOLCHAIN#go}" >"${TEST_ROOT}/go.mod"
printf 'package verificationfixture\nfunc Value() int { return 42 }\n' >"${TEST_ROOT}/fixture.go"
gofmt -w "${TEST_ROOT}/fixture.go"
(cd "${TEST_ROOT}" && "${LINTER}" run --config "${ROOT}/.golangci.yml" ./...)
for CASE in errcheck staticcheck; do
  case "${CASE}" in
    errcheck) printf 'package verificationfixture\nimport "io"\nfunc Close(c io.Closer) { c.Close() }\n' ;;
    staticcheck) printf 'package verificationfixture\nimport "regexp"\nfunc Match(s string) (bool, error) { return regexp.MatchString("[a-", s) }\n' ;;
  esac >"${TEST_ROOT}/fixture.go"
  gofmt -w "${TEST_ROOT}/fixture.go"
  STATUS=0
  (cd "${TEST_ROOT}" && "${LINTER}" run --config "${ROOT}/.golangci.yml" ./...) >"${TEST_ROOT}/lint.log" 2>&1 || STATUS=$?
  [[ "${STATUS}" != 0 ]]
  grep -q "(${CASE})" "${TEST_ROOT}/lint.log"
done
reject_repository_binary_write() {
  grep -En '^[[:space:]]*((cp|mv|rm|mkdir|go build|printf|cat)[[:space:]].*\$\{MAILCLI_ROOT\}/bin|((export[[:space:]]+)?BUILD_OUTPUT|PREEXISTING_BINARY)=.*\$\{MAILCLI_ROOT\}/bin)' "$@"
}
if reject_repository_binary_write "${ROOT}/scripts/tests/"*.sh; then
  printf 'Tests must not write or alias the repository binary\n' >&2
  exit 1
fi
for MUTANT in \
  'go build -o "${MAILCLI_ROOT}/bin/mailcli" ./cmd/mailcli' \
  'BUILD_OUTPUT="${MAILCLI_BUILD_OUTPUT:-${MAILCLI_ROOT}/bin/mailcli}"' \
  'PREEXISTING_BINARY="${MAILCLI_ROOT}/bin/mailcli"'; do
  printf '%s\n' "${MUTANT}" >"${TEST_ROOT}/binary-write.sh"
  reject_repository_binary_write "${TEST_ROOT}/binary-write.sh" >/dev/null
done
grep -Fq 'go build -mod=readonly -o "${MAILCLI_BUILD_OUTPUT}" ./cmd/mailcli' "${ROOT}/scripts/tests/test.sh"
grep -Fq 'MAILCLI_BINARY="${MAILCLI_BUILD_OUTPUT}" run_shell_test scripts/tests/test-live-responsiveness.sh' "${ROOT}/scripts/tests/test.sh"
grep -Fqx 'BUILD_OUTPUT="${TEST_ROOT}/build/mailcli"' "${ROOT}/scripts/tests/test-skill-drift.sh"
go test -vet=off -count=1 "${ROOT}/internal/cli" -run '^TestCIRunnerDocumentationMatchesWorkflow$'
printf 'Verification policy passed: exact toolchain, real lint diagnostics, fail-closed scanner classification\n'

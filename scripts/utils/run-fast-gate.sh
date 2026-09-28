#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -ge 4 && "$#" -le 6 && ( "$4" == --fast || "$4" == --lint-only ) ]] || {
  printf 'Usage: run-fast-gate.sh ROOT BASELINE INDEX_TREE --fast|--lint-only [LINT_RECEIPT] [TASK_IDS]\n' >&2
  exit 2
}
SOURCE_ROOT="$1"
BASELINE_HEAD="$2"
INDEX_TREE="$3"
MODE="$4"
LINT_RECEIPT="${5:-}"
TASK_IDS="${6:-}"
MAILCLI_TEST_CPUS="${MAILCLI_TEST_CPUS:-4}"
MAILCLI_TEST_PACKAGES="${MAILCLI_TEST_PACKAGES:-4}"
for VALUE in "${MAILCLI_TEST_CPUS}" "${MAILCLI_TEST_PACKAGES}"; do
  [[ "${VALUE}" =~ ^[1-9][0-9]*$ ]] || { printf 'Verification concurrency must be a positive integer\n' >&2; exit 2; }
done
export GOMAXPROCS="${MAILCLI_TEST_CPUS}"
for VARIABLE in $(git rev-parse --local-env-vars); do unset "${VARIABLE}"; done
unset MAILCLI_WRITE_ROOT MAILCLI_GATE_RECEIPTS
export GOWORK=off
umask 077
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-fast-gate.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
PRODUCT_ROOT="${TEST_ROOT}/product"
git clone -q --shared --no-checkout --no-tags "${SOURCE_ROOT}" "${PRODUCT_ROOT}"
PRODUCT_ROOT="$(cd "${PRODUCT_ROOT}" && pwd -P)"
git -C "${PRODUCT_ROOT}" checkout -q --detach "${BASELINE_HEAD}"
source "${PRODUCT_ROOT}/scripts/utils/check-go-toolchain.sh"
cd "${PRODUCT_ROOT}"
(unset GOTOOLCHAIN; check_go_toolchain "${PRODUCT_ROOT}" >&2; go list -mod=readonly -deps -test -e -json ./...) >"${TEST_ROOT}/baseline-packages"
git read-tree --reset -u "${INDEX_TREE}"
check_go_toolchain "${PRODUCT_ROOT}"
PRODUCT_REFS="$(git for-each-ref --format='%(refname) %(objectname)')"
go list -mod=readonly -deps -test -e -json ./... >"${TEST_ROOT}/staged-packages"
git diff --name-only -z --no-renames "${BASELINE_HEAD}" "${INDEX_TREE}" -- >"${TEST_ROOT}/changed-paths"
go run -mod=readonly ./scripts/utils/select-gate-packages "${PRODUCT_ROOT}" \
  "${TEST_ROOT}/baseline-packages" "${TEST_ROOT}/staged-packages" "${TEST_ROOT}/changed-paths" >"${TEST_ROOT}/plan"
PACKAGES=()
while IFS= read -r PACKAGE; do PACKAGES+=("${PACKAGE}"); done < <(jq -r '.Packages[]' "${TEST_ROOT}/plan")
LINT_PACKAGES=()
CLI_SELECTED=false
if [[ "${#PACKAGES[@]}" -gt 0 ]]; then
  go list -mod=readonly -f '{{.Dir}}' "${PACKAGES[@]}" >"${TEST_ROOT}/lint-directories"
  while IFS= read -r DIRECTORY; do
    [[ "${DIRECTORY}" != "${PRODUCT_ROOT}/internal/cli" ]] || CLI_SELECTED=true
    if [[ "${DIRECTORY}" == "${PRODUCT_ROOT}" ]]; then
      LINT_PACKAGES+=(.)
    elif [[ "${DIRECTORY}" == "${PRODUCT_ROOT}/"* ]]; then
      LINT_PACKAGES+=("./${DIRECTORY#"${PRODUCT_ROOT}/"}")
    else
      printf 'Selected package escaped the staged product: %s\n' "${DIRECTORY}" >&2
      exit 1
    fi
  done <"${TEST_ROOT}/lint-directories"
  [[ "${#LINT_PACKAGES[@]}" == "${#PACKAGES[@]}" ]] || exit 1
fi
if [[ "$(jq -r '.LintAll' "${TEST_ROOT}/plan")" == true ]]; then LINT_PACKAGES=(./...); fi

LINTER="$(command -v golangci-lint || true)"
[[ -n "${LINTER}" ]] || LINTER="$(go env GOPATH)/bin/golangci-lint"
[[ -x "${LINTER}" ]] || { printf 'Configured golangci-lint is required\n' >&2; exit 2; }
LINT_IDENTITY="$(
  {
    printf '%s\n%s\n' "${BASELINE_HEAD}" "${INDEX_TREE}"
    shasum -a 256 "${LINTER}" "$0" | awk '{print $1}'
    go env -json GOVERSION GOOS GOARCH CGO_ENABLED GOFLAGS GOWORK CC CXX CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS
    [[ "${#LINT_PACKAGES[@]}" == 0 ]] || printf '%s\n' "${LINT_PACKAGES[@]}"
    printf 'cpus=%s\npackages=%s\n' "${MAILCLI_TEST_CPUS}" "${MAILCLI_TEST_PACKAGES}"
  } | shasum -a 256 | awk '{print $1}'
)"
LINT_REUSED=false
if [[ -n "${LINT_RECEIPT}" && -f "${LINT_RECEIPT}" && ! -L "${LINT_RECEIPT}" &&
  "$(cat "${LINT_RECEIPT}")" == "${LINT_IDENTITY}" ]]; then
  LINT_REUSED=true
else
  while IFS= read -r -d '' PATH_NAME; do
    if [[ -f "${PATH_NAME}" && "${PATH_NAME}" == *.go ]]; then
      UNFORMATTED="$(gofmt -l "${PATH_NAME}")"
      [[ -z "${UNFORMATTED}" ]] || { printf 'Unformatted staged Go source: %s\n' "${UNFORMATTED}" >&2; exit 1; }
    fi
    if [[ -f "${PATH_NAME}" && "${PATH_NAME}" == *.sh ]]; then
      [[ -x "${PATH_NAME}" ]] || { printf 'Staged shell script is not executable: %s\n' "${PATH_NAME}" >&2; exit 1; }
      bash -n "${PATH_NAME}"
    fi
  done <"${TEST_ROOT}/changed-paths"
  if [[ "${#LINT_PACKAGES[@]}" -gt 0 ]]; then
    "${LINTER}" run --config "${PRODUCT_ROOT}/.golangci.yml" --concurrency "${MAILCLI_TEST_CPUS:-4}" "${LINT_PACKAGES[@]}"
  fi
fi
printf 'lint_tree=%s\nlint_identity=%s\nlint_reused=%s\n' "${INDEX_TREE}" "${LINT_IDENTITY}" "${LINT_REUSED}"
if [[ "${MODE}" == --fast ]]; then
  if [[ "$(jq -r '.VerifyModules' "${TEST_ROOT}/plan")" == true ]]; then go mod verify; fi
  if [[ "${#PACKAGES[@]}" -gt 0 ]]; then
    printf 'selected_package=%s\n' "${PACKAGES[@]}"
    MAILCLI_ROOT="${PRODUCT_ROOT}" MAILCLI_LIVE_TESTS= MAILCLI_KEYCHAIN_LIVE= \
      go test -mod=readonly -vet=off -count=1 -p "${MAILCLI_TEST_PACKAGES:-4}" "${PACKAGES[@]}"
  fi
  if [[ "${CLI_SELECTED}" == false && "$(jq -r '.Documentation' "${TEST_ROOT}/plan")" == true ]]; then
    MAILCLI_ROOT="${PRODUCT_ROOT}" MAILCLI_LIVE_TESTS= MAILCLI_KEYCHAIN_LIVE= \
      go test -mod=readonly -vet=off -count=1 ./internal/cli -run '^Test.*(Documentation|Skill)'
  fi
fi
[[ "$(git rev-parse HEAD)" == "${BASELINE_HEAD}" && "$(git write-tree)" == "${INDEX_TREE}" ]]
git diff --quiet
[[ -z "$(git ls-files --others --exclude-standard)" ]]
[[ -z "$(git ls-files -v | grep -E '^[a-zS] ' || true)" ]]
[[ "$(git for-each-ref --format='%(refname) %(objectname)')" == "${PRODUCT_REFS}" ]]
if [[ -n "${LINT_RECEIPT}" && "${LINT_REUSED}" == false ]]; then
  [[ ! -L "${LINT_RECEIPT}" ]] || { printf 'Lint receipt must not be a symlink\n' >&2; exit 1; }
  printf '%s\n' "${LINT_IDENTITY}" >"${LINT_RECEIPT}"
fi
if [[ "${MODE}" == --fast ]]; then
  CHECKS=(scripts/tests/test-commit-authority.sh)
  while IFS= read -r CHECK; do
    [[ "${CHECK}" == scripts/tests/test-commit-authority.sh ]] || CHECKS+=("${CHECK}")
  done < <(jq -r '.ShellChecks[]' "${TEST_ROOT}/plan")
  "$(dirname "${BASH_SOURCE[0]}")/run-staged-gate.sh" "${SOURCE_ROOT}" "${BASELINE_HEAD}" "${INDEX_TREE}" "${TASK_IDS}" --checks "${CHECKS[@]}"
  printf 'gate_tier=fast\nfull_gate=deferred\n'
fi

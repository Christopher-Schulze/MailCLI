#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-preflight-test.XXXXXX")"
cleanup() {
  if [[ "${TEST_ROOT}" == *"/mailcli-preflight-test."* && -d "${TEST_ROOT}" ]]; then
    rm -rf "${TEST_ROOT}"
  fi
}
trap cleanup EXIT

FAKE_BINARY="${TEST_ROOT}/mailcli"
COUNT_ROOT="${TEST_ROOT}/counts"
CACHE_ROOT="${TEST_ROOT}/cache"
mkdir -p "${COUNT_ROOT}"
cat >"${FAKE_BINARY}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

increment() {
  local path="${FAKE_COUNT_ROOT}/$1"
  local count=0
  if [[ -f "${path}" ]]; then
    count="$(<"${path}")"
  fi
  printf '%s\n' "$((count + 1))" >"${path}"
}

case "${1:-}" in
  version)
    printf 'mailcli 9.9.9\n'
    ;;
  capabilities)
    increment capabilities
    printf '%s\n' '{"schema_version":1,"ok":true,"command":"capabilities","data":{"capabilities":{"schema_version":1}},"error":null}'
    ;;
  doctor)
    increment doctor
    if [[ "${FAKE_DOCTOR_FAIL:-0}" == "1" ]]; then
      printf '%s\n' '{"schema_version":1,"ok":false,"command":"doctor","data":{},"error":{"code":"operation_failed"}}'
      exit 1
    fi
    printf '%s\n' '{"schema_version":1,"ok":true,"command":"doctor","data":{"checks":[]},"error":null}'
    ;;
  *)
    printf 'unexpected fake command\n' >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "${FAKE_BINARY}"

export FAKE_COUNT_ROOT="${COUNT_ROOT}"
preflight() {
  "${MAILCLI_ROOT}/scripts/utils/mailcli-preflight.sh" \
    "$@" --binary "${FAKE_BINARY}" --cache-dir "${CACHE_ROOT}"
}
count() {
  local name="$1"
  if [[ -f "${COUNT_ROOT}/${name}" ]]; then
    cat "${COUNT_ROOT}/${name}"
  else
    printf '0\n'
  fi
}
expect_count() {
  local name="$1"
  local expected="$2"
  local actual
  actual="$(count "${name}")"
  [[ "${actual}" == "${expected}" ]] || {
    printf '%s calls = %s, want %s\n' "${name}" "${actual}" "${expected}" >&2
    exit 1
  }
}

preflight capabilities | grep -Fq '"schema_version":1'
expect_count capabilities 1
preflight capabilities | grep -Fq '"command":"capabilities"'
expect_count capabilities 1

preflight doctor | grep -Fq '"command":"doctor"'
expect_count doctor 1
preflight doctor >/dev/null
expect_count doctor 1

if FAKE_DOCTOR_FAIL=1 preflight doctor --refresh >/dev/null 2>&1; then
  printf 'failed doctor unexpectedly succeeded\n' >&2
  exit 1
fi
expect_count doctor 2
preflight doctor >/dev/null
expect_count doctor 3

printf '\n# binary identity changed\n' >>"${FAKE_BINARY}"
preflight capabilities >/dev/null
expect_count capabilities 2

preflight invalidate
preflight capabilities >/dev/null
expect_count capabilities 3
printf 'Preflight cache passed: binary/schema identity, bounded doctor reuse, refresh, and failure invalidation\n'

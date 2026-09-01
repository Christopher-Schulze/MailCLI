#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAILCLI_BINARY="${MAILCLI_BINARY:-${MAILCLI_ROOT}/bin/mailcli}"
MAILCLI_MAX_POST_SECONDS="${MAILCLI_MAX_POST_SECONDS:-2.0}"

if [[ ! -x "${MAILCLI_BINARY}" ]]; then
  printf 'MailCLI binary is not executable: %s\n' "${MAILCLI_BINARY}" >&2
  exit 1
fi

MAIL_EXECUTABLE="/System/Applications/Mail.app/Contents/MacOS/Mail"
MAIL_PIDS="$(pgrep -f -x "${MAIL_EXECUTABLE}" || true)"
if [[ -z "${MAIL_PIDS}" || "${MAIL_PIDS}" == *$'\n'* ]]; then
  printf 'Responsiveness gate requires exactly one running Mail process\n' >&2
  exit 1
fi
MAIL_PID="${MAIL_PIDS}"

compose_count() {
  /usr/bin/osascript -l JavaScript -e "
ObjC.import('AppKit');
const processID = ${MAIL_PID};
const running = $.NSRunningApplication.runningApplicationWithProcessIdentifier(processID);
if (ObjC.unwrap(running.bundleIdentifier) !== 'com.apple.mail') throw new Error('Mail identity changed');
Application(processID).outgoingMessages().length;
"
}

BASELINE_COMPOSE_COUNT="$(compose_count)"

if pgrep -x mailcli >/dev/null || pgrep -x osascript >/dev/null; then
  printf 'Responsiveness gate requires no pre-existing mailcli or osascript process\n' >&2
  exit 1
fi

MAILCLI_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-responsiveness.XXXXXX")"
cleanup() {
  rm -rf "${MAILCLI_TEMP}"
}
trap cleanup EXIT

assert_quiescent() {
  for _ in {1..40}; do
    if ! pgrep -x mailcli >/dev/null && ! pgrep -x osascript >/dev/null; then
      break
    fi
    sleep 0.05
  done
  if pgrep -x mailcli >/dev/null || pgrep -x osascript >/dev/null; then
    printf 'MailCLI or osascript remained after the completed command\n' >&2
    exit 1
  fi
  if [[ "$(pgrep -f -x "${MAIL_EXECUTABLE}" || true)" != "${MAIL_PID}" ]]; then
    printf 'Mail process changed during the responsiveness gate\n' >&2
    exit 1
  fi
  if lsof -p "${MAIL_PID}" 2>/dev/null | grep -F "${MAILCLI_ROOT}" >/dev/null; then
    printf 'Mail retained an open handle into the MailCLI repository\n' >&2
    exit 1
  fi
  if [[ "$(compose_count)" != "${BASELINE_COMPOSE_COUNT}" ]]; then
    printf 'Mail compose object count changed during the responsiveness gate\n' >&2
    exit 1
  fi
}

run_probe() {
  local name="$1"
  local output="${MAILCLI_TEMP}/${name}.json"
  local timing="${MAILCLI_TEMP}/${name}.time"
  /usr/bin/time -p "${MAILCLI_BINARY}" doctor --live --json >"${output}" 2>"${timing}"
  if ! grep -Eq '"ok"[[:space:]]*:[[:space:]]*true' "${output}"; then
    printf 'Live probe did not return ok=true: %s\n' "${name}" >&2
    exit 1
  fi
  assert_quiescent
  awk '$1 == "real" {print $2}' "${timing}"
}

BASELINE_SECONDS="$(run_probe baseline)"
OPERATION_SECONDS="$(run_probe operation)"
POST_SECONDS="$(run_probe post)"

if ! awk -v baseline="${BASELINE_SECONDS}" -v post="${POST_SECONDS}" \
  -v absolute="${MAILCLI_MAX_POST_SECONDS}" 'BEGIN {
    relative = baseline * 2.0 + 0.1
    if (relative < 0.5) relative = 0.5
    exit !(post <= absolute && post <= relative)
  }'; then
  printf 'Mail responsiveness regressed: baseline=%ss post=%ss\n' \
    "${BASELINE_SECONDS}" "${POST_SECONDS}" >&2
  exit 1
fi

printf 'Mail responsiveness gate passed: pid=%s baseline=%ss operation=%ss post=%ss compose_objects=%s; no residual processes or handles\n' \
  "${MAIL_PID}" "${BASELINE_SECONDS}" "${OPERATION_SECONDS}" "${POST_SECONDS}" "${BASELINE_COMPOSE_COUNT}"

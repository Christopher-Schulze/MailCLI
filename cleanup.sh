#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

fail() { printf 'Cleanup refused: %s\n' "$1" >&2; exit 1; }
[[ "$#" == 0 ]] || { printf 'Usage: ./cleanup.sh\n' >&2; exit 2; }
[[ ! -L "${BASH_SOURCE[0]}" ]] || fail 'the script must not be a symbolic link'
ROOT="$(CDPATH= cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for VARIABLE in $(git rev-parse --local-env-vars); do unset "${VARIABLE}"; done
GIT_ROOT="$(git -C "${ROOT}" rev-parse --show-toplevel)" || fail 'a Git checkout is required'
[[ "${GIT_ROOT}" -ef "${ROOT}" ]] || fail 'the script must be at the checkout root'
GIT_DIRECTORY="$(git -C "${ROOT}" rev-parse --absolute-git-dir)"
[[ ! -e "${GIT_DIRECTORY}/mailcli-write-lease" && ! -L "${GIT_DIRECTORY}/mailcli-write-lease" ]] ||
  fail 'a writer lease is active'

size_kib() {
  local OUTPUT
  OUTPUT="$(du -sk "$1")" || return 1
  printf '%s\n' "${OUTPUT%%[[:space:]]*}"
}

format_size() {
  awk -v kib="$1" 'BEGIN {
    magnitude = kib < 0 ? -kib : kib
    if (magnitude < 1024) printf "%.0f KiB", kib
    else if (magnitude < 1048576) printf "%.2f MiB", kib / 1024
    else printf "%.2f GiB", kib / 1048576
  }'
}

validate_target() {
  local TARGET="$1" RELATIVE="${1#"${ROOT}/"}" TRACKED LINKS
  if [[ -L "${TARGET}" ]]; then
    printf 'Cleanup refused: symbolic link: %q\n' "${RELATIVE}" >&2; return 1
  fi
  case "${RELATIVE}" in
    bin | dist | graphify-out | .coverage)
      [[ -d "${TARGET}" ]] || { printf 'Cleanup refused: expected directory: %q\n' "${RELATIVE}" >&2; return 1; }
      ;;
    *)
      [[ -f "${TARGET}" ]] || { printf 'Cleanup refused: expected regular file: %q\n' "${RELATIVE}" >&2; return 1; }
      ;;
  esac
  TRACKED="$(git -C "${ROOT}" ls-files -- ":(literal)${RELATIVE}")" || return 1
  if [[ -n "${TRACKED}" ]]; then
    printf 'Cleanup refused: tracked content: %q\n' "${RELATIVE}" >&2; return 1
  fi
  LINKS="$(find "${TARGET}" -type l -print -quit)" || return 1
  if [[ -n "${LINKS}" ]]; then
    printf 'Cleanup refused: nested symbolic link: %q\n' "${RELATIVE}" >&2; return 1
  fi
}

shopt -s nullglob
CANDIDATES=(
  "${ROOT}/bin" "${ROOT}/dist" "${ROOT}/graphify-out" "${ROOT}/.coverage"
  "${ROOT}"/coverage*.out "${ROOT}"/*.test "${ROOT}"/*.prof
  "${ROOT}"/*.pprof "${ROOT}"/*.trace
)
TARGETS=() SIZES=() NAMES=()
# Validate every eligible path before the first deletion.
for TARGET in "${CANDIDATES[@]}"; do
  [[ -e "${TARGET}" || -L "${TARGET}" ]] || continue
  validate_target "${TARGET}" || exit 1
  SIZE="$(size_kib "${TARGET}")" || fail 'could not measure a cleanup target'
  NAME="${TARGET#"${ROOT}/"}"
  [[ ! -d "${TARGET}" ]] || NAME="${NAME}/"
  TARGETS+=("${TARGET}") SIZES+=("${SIZE}") NAMES+=("${NAME}")
done

BEFORE="$(size_kib "${ROOT}")" || fail 'could not measure the repository'
printf 'Repository before: %s\n' "$(format_size "${BEFORE}")"
STATUS=0
for ((INDEX = 0; INDEX < ${#TARGETS[@]}; INDEX++)); do
  TARGET="${TARGETS[INDEX]}"
  if ! validate_target "${TARGET}"; then STATUS=1; break; fi
  if rm -rfv -- "${TARGET}" && [[ ! -e "${TARGET}" && ! -L "${TARGET}" ]]; then
    printf 'Deleted: %q (%s)\n' "${NAMES[INDEX]}" "$(format_size "${SIZES[INDEX]}")"
  else
    printf 'Cleanup failed: %q\n' "${NAMES[INDEX]}" >&2
    STATUS=1
    break
  fi
done
AFTER="$(size_kib "${ROOT}")" || fail 'could not measure the remaining repository'
printf 'Freed in repository: %s\n' "$(format_size "$((BEFORE - AFTER))")"
printf 'Repository after: %s\n' "$(format_size "${AFTER}")"
exit "${STATUS}"

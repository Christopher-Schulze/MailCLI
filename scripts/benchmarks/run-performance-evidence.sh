#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BENCHMARK_REPETITIONS=20
BENCHMARK_CPUS=4
RESULT_DIRECTORY="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-performance-evidence.XXXXXX")"
trap 'rm -rf "${RESULT_DIRECTORY}"' EXIT

summarize_results() {
  local RESULT_PATH="$1"
  local EXPECTED_REPETITIONS="$2"
  local SAMPLE_PATH="${RESULT_PATH}.samples"

  awk '
    /^Benchmark/ {
      name = $1
      sub(/-[0-9]+$/, "", name)
      ns = ""
      bytes = ""
      allocs = ""
      for (field = 2; field < NF; field++) {
        if ($(field + 1) == "ns/op") ns = $field
        if ($(field + 1) == "B/op") bytes = $field
        if ($(field + 1) == "allocs/op") allocs = $field
      }
      if (ns != "" && bytes != "" && allocs != "") {
        printf "%s\t%.3f\t%.3f\t%.3f\n", name, ns, bytes, allocs
      }
    }
  ' "${RESULT_PATH}" | LC_ALL=C sort -t $'\t' -k1,1 -k2,2n >"${SAMPLE_PATH}"

  [[ -s "${SAMPLE_PATH}" ]] || {
    printf 'No benchmark samples were recorded in %s\n' "${RESULT_PATH}" >&2
    return 1
  }

  awk -F '\t' -v expected="${EXPECTED_REPETITIONS}" '
    {
      name = $1
      if (!(name in seen)) {
        seen[name] = 1
        names[++name_count] = name
      }
      count[name]++
      sample = count[name]
      ns[name, sample] = $2
      bytes[name, sample] = $3
      allocs[name, sample] = $4
    }
    END {
      failed = 0
      for (name_index = 1; name_index <= name_count; name_index++) {
        name = names[name_index]
        samples = count[name]
        if (samples != expected) {
          printf "Benchmark %s recorded %d samples, want %d\n", name, samples, expected > "/dev/stderr"
          failed = 1
          continue
        }
        p50 = int((samples + 1) / 2)
        p95 = int((95 * samples + 99) / 100)
        printf "summary benchmark=%s repetitions=%d p50_ns/op=%.0f p95_ns/op=%.0f p50_B/op=%.0f p50_allocs/op=%.0f\n",
          name, samples, ns[name, p50], ns[name, p95], bytes[name, p50], allocs[name, p50]
      }
      exit failed
    }
  ' "${SAMPLE_PATH}"
}

run_group() {
  local NAME="$1"
  local INPUT_SHAPE="$2"
  local PACKAGE="$3"
  local REGEX="$4"
  local BENCHTIME="$5"
  local RESULT_PATH="${RESULT_DIRECTORY}/${NAME}.txt"

  printf '\ngroup=%s\ninput=%s\npackage=%s\nregex=%s\nbenchtime=%s\nrepetitions=%d\n' \
    "${NAME}" "${INPUT_SHAPE}" "${PACKAGE}" "${REGEX}" "${BENCHTIME}" "${BENCHMARK_REPETITIONS}"
  go test -run '^$' -bench "${REGEX}" -benchmem -benchtime "${BENCHTIME}" \
    -count "${BENCHMARK_REPETITIONS}" -cpu "${BENCHMARK_CPUS}" -timeout 10m "${PACKAGE}" |
    tee "${RESULT_PATH}"
  summarize_results "${RESULT_PATH}" "${BENCHMARK_REPETITIONS}"
}

cd "${MAILCLI_ROOT}"
GIT_WORKTREE_STATE=clean
if ! git diff --quiet || [[ -n "$(git ls-files --others --exclude-standard)" ]]; then
  GIT_WORKTREE_STATE=dirty
fi
printf 'MailCLI deterministic performance evidence\n'
printf 'environment.git_head=%s\n' "$(git rev-parse HEAD)"
printf 'environment.git_worktree=%s\n' "${GIT_WORKTREE_STATE}"
printf 'environment.go=%s\n' "$(go version)"
printf 'environment.os=%s\n' "$(uname -srv)"
printf 'environment.arch=%s\n' "$(uname -m)"
printf 'environment.cpu=%s\n' "$(sysctl -n machdep.cpu.brand_string 2>/dev/null || printf unknown)"
printf 'environment.logical_cpus=%s\n' "$(sysctl -n hw.logicalcpu 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || printf unknown)"
printf 'benchmark_cpus=%d\n' "${BENCHMARK_CPUS}"
printf 'p95_definition=nearest-rank p95 across repeated benchmark-run ns/op means\n'
printf 'safety=generated fixtures and loopback fake servers only; no Mail.app, credentials, network providers, or user Mail store\n'

run_group \
  mime-small \
  '4 KiB body with no attachment, 64 KiB attachment, and 1 MiB attachment' \
  ./internal/mail \
  '^BenchmarkBuildMessage(Plain4KiB|Attachment64KiB|Attachment1MiB)$' \
  50x
run_group \
  attachment-large \
  '4 KiB body plus one 64 MiB attachment; compatibility, redundant-read baseline, and production streaming paths' \
  ./internal/mail \
  '^Benchmark(BuildMessageAttachment64MiB|SendAttachment(DoubleRead64MiB|Streaming64MiB))$' \
  1x
run_group \
  mime-read \
  'one multipart message with a 1 MiB binary attachment; skip and full MIME parsing' \
  ./internal/mailstore \
  '^BenchmarkSkipVsFullAttachment1MiB$' \
  5x
run_group \
  search-fixture \
  'generated 603-message store; first 25-result metadata, default body, and explicit exact-count body pages' \
  ./internal/mailstore \
  '^BenchmarkSearchFixture603$' \
  20x
run_group \
  search-fold \
  '1 MiB lowercase and mixed ASCII plus decomposed Unicode; the shared SQL and body search folding policy' \
  ./internal/mailstore \
  '^BenchmarkSearchFold$' \
  5x
run_group \
  catalog-shortcut \
  '256 generated messages with 1 MiB attachments; catalog-proven search versus MIME-scan baseline' \
  ./internal/mailstore \
  '^BenchmarkAttachmentCatalogShortcut$' \
  1x
run_group \
  rich-content \
  '100 KiB repeated tags; rich HTML, plain text, links, captioned tables, Markdown, depth-250 content, and a 1 MB text body under one or 250 removed wrappers' \
  ./internal/mail \
  '^BenchmarkPrepareDraftContent$' \
  10x
run_group \
  draft-list \
  '1-byte and 1 MiB draft bodies; full-record baseline, streaming summary, and paginated service; 25-entry pages over 1 and 2049 records; file bytes and serialized output are separate metrics' \
  ./internal/mail \
  '^BenchmarkDraft(SummaryBodies|ListPage)$' \
  5x
run_group \
  projected-output \
  'tiny, 1 MiB and 8 MiB raw JSON; accepted and oversized output including finalization' \
  ./internal/cli \
  '^BenchmarkProjectedRawOutput$' \
  5x
run_group \
  imap-concurrency \
  'two concurrent STATUS or FETCH operations against a loopback fake server with 2 ms command delay and one or two sessions' \
  ./internal/transport/imapclient \
  '^Benchmark(IndependentStatusConcurrency|FetchConcurrency)$' \
  20x

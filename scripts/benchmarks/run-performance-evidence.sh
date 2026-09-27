#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BENCHMARK_REPETITIONS=20
BENCHMARK_CPUS=4
RESULT_DIRECTORY="$(mktemp -d "${TMPDIR:-/tmp}/mailcli-performance-evidence.XXXXXX")"
SELECTED_GROUP=""
SELECTED_GROUP_FOUND=false
trap 'rm -rf "${RESULT_DIRECTORY}"' EXIT
source "${MAILCLI_ROOT}/scripts/benchmarks/summarize-performance-evidence.sh"

usage() {
  printf '%s\n' \
    'Usage: run-performance-evidence.sh [--group NAME]' \
    '       run-performance-evidence.sh --help' \
    'Run the complete matrix by default, or select one named group.'
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
  usage
  exit 0
fi
if [[ "$#" -gt 0 ]]; then
  [[ "$#" -eq 2 && "$1" == "--group" && -n "$2" ]] || { usage >&2; exit 2; }
  SELECTED_GROUP="$2"
fi

run_group() {
  local NAME="$1"
  local INPUT_SHAPE="$2"
  local PACKAGE="$3"
  local REGEX="$4"
  local BENCHTIME="$5"
  local REPETITIONS="${6:-${BENCHMARK_REPETITIONS}}"
  local RESULT_PATH="${RESULT_DIRECTORY}/${NAME}.txt"

  if [[ -n "${SELECTED_GROUP}" && "${NAME}" != "${SELECTED_GROUP}" ]]; then
    return 0
  fi
  SELECTED_GROUP_FOUND=true

  printf '\ngroup=%s\ninput=%s\npackage=%s\nregex=%s\nbenchtime=%s\nrepetitions=%d\n' \
    "${NAME}" "${INPUT_SHAPE}" "${PACKAGE}" "${REGEX}" "${BENCHTIME}" "${REPETITIONS}"
  go test -run '^$' -bench "${REGEX}" -benchmem -benchtime "${BENCHTIME}" \
    -count "${REPETITIONS}" -cpu "${BENCHMARK_CPUS}" -timeout 10m "${PACKAGE}" |
    tee "${RESULT_PATH}"
  summarize_benchmark_results "${RESULT_PATH}" "${REPETITIONS}"
}

cd "${MAILCLI_ROOT}"
source "${MAILCLI_ROOT}/scripts/utils/check-go-toolchain.sh"
check_go_toolchain "${MAILCLI_ROOT}"
GIT_WORKTREE_STATE=clean
if ! git diff --quiet || ! git diff --cached --quiet || [[ -n "$(git ls-files --others --exclude-standard)" ]]; then
  GIT_WORKTREE_STATE=dirty
fi
printf 'MailCLI deterministic performance evidence\n'
printf 'environment.git_head=%s\n' "$(git rev-parse HEAD)"
printf 'environment.git_worktree=%s\n' "${GIT_WORKTREE_STATE}"
printf 'environment.go=%s\n' "$(go version)"
printf 'environment.goflags=%s\n' "$(go env GOFLAGS)"
printf 'environment.cgo_enabled=%s\n' "$(go env CGO_ENABLED)"
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
  body-large \
  '4 MiB ASCII and Unicode bodies through real spool composition; Unicode spool buffers at 32/128/256/512 KiB and 1 MiB' \
  ./internal/mail \
  '^Benchmark(LargeBodySpool|BodySpoolBuffer)$' \
  3x
run_group \
  attachment-large \
  '4 KiB body plus one 64 MiB attachment; compatibility, redundant-read baseline, and production streaming paths' \
  ./internal/mail \
  '^Benchmark(BuildMessageAttachment64MiB|SendAttachment(DoubleRead64MiB|Streaming64MiB))$' \
  1x
run_group \
  mime-read \
  '1 MiB attachment skip/full MIME parsing; small/medium/maximum headers; 4 MiB attachment hydration with owned-string and reader paths' \
  ./internal/mailstore \
  '^Benchmark(SkipVsFullAttachment1MiB|ReadRawHeaders|HydratedSource)$' \
  5x
run_group \
  attachment-discovery \
  '1024 external entries with one name match and 128 identical ambiguity candidates' \
  ./internal/mailstore \
  '^BenchmarkExternalAttachmentDiscovery$' \
  5x
run_group \
  search-fixture \
  'generated 603-message store; first 25-result metadata, default body, and explicit exact-count body pages' \
  ./internal/mailstore \
  '^BenchmarkSearchFixture603$' \
  20x
run_group \
  open_warm_cache \
  'first Store.Open after writing a generated 600-message SQLite/EMLX fixture; OS page-cache entries are not evicted' \
  ./internal/mailstore \
  '^BenchmarkGeneratedStoreOpenWarmCache$' \
  1x
run_group \
  generated-store-lifecycle \
  '600 messages, two active accounts and three indexed mailboxes; repeated Open/Close, account/mailbox lists, exact 25-message page and full message read' \
  ./internal/mailstore \
  '^BenchmarkGeneratedStoreLifecycle$' \
  20x
run_group \
  mutation-account-resolution \
  'generated 600-message/two-account store; one or 100 per-item mutation target resolutions with file-backed bindings and a fake IMAP boundary' \
  ./internal/mailstore \
  '^BenchmarkMutationAccountResolution$' \
  1x
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
  plaintext-renderer \
  'ordinary, dense, link-heavy and large-text parsed trees; streaming output versus the frozen token-buffered reference' \
  ./internal/mail \
  '^BenchmarkPlainTextRendering$' \
  5x
run_group \
  content-ownership \
  'ordinary and 2 MiB trimmed plaintext; single, repeated and distinct content diagnostics; retained_B is net process heap after GC' \
  ./internal/mail \
  '^Benchmark(PlainTextOwnership|ContentDiagnosticKeys)$' \
  5x
run_group \
  mime-ownership \
  'ordinary, trimmed, empty and untrimmed 4 MiB plain MIME text; retained_B is net process heap after GC' \
  ./internal/mailstore \
  '^BenchmarkMIMETextOwnership$' \
  5x
run_group \
  raw-source-builder \
  'generated exact 8, 32 and 64 MiB EMLX sources through Store.GetRawSource; one operation per sample' \
  ./internal/mailstore \
  '^BenchmarkRawSourceBuilder$' \
  1x
run_group \
  list-summary-read \
  'generated 25-message pages with short and 1 MiB summary rows; one operation per sample' \
  ./internal/mailstore \
  '^BenchmarkListSummaryRead$' \
  1x
run_group \
  mailbox-list-plan \
  '16 cases: 10k/100k messages, labels absent/present, first/later 25-item page, forced CTE/production probe; 20 operation samples per case' \
  ./internal/mailstore \
  '^BenchmarkMailboxListPlan$' \
  20x \
  1
run_group \
  incoming-html \
  'ordinary, dense 512 KiB, and excessive-token received HTML; legacy and context-bounded conversion' \
  ./internal/mail \
  '^BenchmarkIncomingHTML$' \
  5x
run_group \
  draft-list \
  '1-byte and 1 MiB draft bodies; full-record baseline, streaming summary, and paginated service; 25-entry pages over 1 and 2049 records; file bytes and serialized output are separate metrics' \
  ./internal/mail \
  '^BenchmarkDraft(SummaryBodies|SummaryBuffer|ListPage)$' \
  5x
run_group \
  draft-directory \
  '0/32/10000 generated draft names through paginated selection; one-pass prune candidate selection over 10000 names' \
  ./internal/mail \
  '^Benchmark(DraftReferenceSelection|DraftPruneCandidateSelection)$' \
  5x
run_group \
  projected-output \
  'tiny, 1 MiB and 8 MiB raw JSON; accepted and oversized output including finalization' \
  ./internal/cli \
  '^BenchmarkProjectedRawOutput$' \
  5x
run_group \
  imap-fetch-memory \
  '4 KiB and 4 MiB TLS FETCH responses; owned bytes versus replayable readers, including consumption' \
  ./internal/transport/imapclient \
  '^BenchmarkFetchPayload$' \
  5x
run_group \
  imap-concurrency \
  'two concurrent STATUS or FETCH operations against a loopback fake server with 2 ms command delay and one or two sessions' \
  ./internal/transport/imapclient \
  '^Benchmark(IndependentStatusConcurrency|FetchConcurrency)$' \
  20x

if [[ -n "${SELECTED_GROUP}" && "${SELECTED_GROUP_FOUND}" != true ]]; then
  printf 'Unknown performance evidence group: %s\n' "${SELECTED_GROUP}" >&2
  exit 2
fi

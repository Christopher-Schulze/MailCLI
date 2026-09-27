#!/usr/bin/env bash
set -euo pipefail

MAILCLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${MAILCLI_ROOT}/scripts/benchmarks/summarize-performance-evidence.sh"
CAPTURED_OUTPUT="$(mktemp "${TMPDIR:-/tmp}/mailcli-benchmark-summary.XXXXXX")"
MISSING_COUNTERS="$(mktemp "${TMPDIR:-/tmp}/mailcli-benchmark-summary-missing.XXXXXX")"
trap 'rm -f -- "${CAPTURED_OUTPUT}" "${MISSING_COUNTERS}"' EXIT

printf '%s\n' \
  'BenchmarkMutationAccountResolution/items_1-4 1 10 ns/op 20 B/op 2 allocs/op 1 catalog_builds/op 2 sent_scan_queries/op 3 binding_loads/op 4 credential_loads/op' \
  'BenchmarkGeneratedStoreInitialOpen-4 1 100 ns/op 200 B/op 20 allocs/op 603 messages 1024 index_B 4096 source_B' \
  'BenchmarkRawSourceBuilder/source_32_MiB-4 1 10 ns/op 32 B/op 2 allocs/op 33554432 source_B 50 custom_count' \
  'BenchmarkSearchBodyHeapRebound-4 1 100 ns/op 200 B/op 20 allocs/op 1.5 retained-B/op' \
  'BenchmarkSummaryRow-4 1 100 ns/op 200 B/op 20 allocs/op 64 summary_row_B' \
  'BenchmarkMutationAccountResolution/items_1-4 1 20 ns/op 40 B/op 4 allocs/op 1 catalog_builds/op 2 sent_scan_queries/op 3 binding_loads/op 4 credential_loads/op' \
  'BenchmarkGeneratedStoreInitialOpen-4 1 300 ns/op 400 B/op 40 allocs/op 603 messages 1024 index_B 4096 source_B' \
  'BenchmarkRawSourceBuilder/source_32_MiB-4 1 30 ns/op 52 B/op 4 allocs/op 33554432 source_B 70 custom_count' \
  'BenchmarkSearchBodyHeapRebound-4 1 300 ns/op 400 B/op 40 allocs/op 2.5 retained-B/op' \
  'BenchmarkSummaryRow-4 1 300 ns/op 400 B/op 40 allocs/op 96 summary_row_B' \
  'BenchmarkMutationAccountResolution/items_1-4 1 30 ns/op 60 B/op 6 allocs/op 1 catalog_builds/op 2 sent_scan_queries/op 3 binding_loads/op 4 credential_loads/op' \
  'BenchmarkGeneratedStoreInitialOpen-4 1 200 ns/op 300 B/op 30 allocs/op 603 messages 1024 index_B 4096 source_B' \
  'BenchmarkRawSourceBuilder/source_32_MiB-4 1 42 ns/op 40 B/op 3 allocs/op 33554432 source_B 60 custom_count' \
  'BenchmarkSearchBodyHeapRebound-4 1 200 ns/op 300 B/op 30 allocs/op 3.5 retained-B/op' \
  'BenchmarkSummaryRow-4 1 200 ns/op 300 B/op 30 allocs/op 80 summary_row_B' \
  'BenchmarkPlain-4 1 100 ns/op 200 B/op 20 allocs/op' \
  'BenchmarkPlain-4 1 300 ns/op 400 B/op 40 allocs/op' \
  'BenchmarkPlain-4 1 200 ns/op 300 B/op 30 allocs/op' \
  >"${CAPTURED_OUTPUT}"

set +o pipefail
ACTUAL="$(summarize_benchmark_results "${CAPTURED_OUTPUT}" 3)"
EXPECTED="$(cat <<'EOF'
summary benchmark=BenchmarkGeneratedStoreInitialOpen repetitions=3 p50_ns/op=200 p95_ns/op=300 p50_B/op=300 p50_allocs/op=30 index_B=1024 messages=603 source_B=4096
summary benchmark=BenchmarkMutationAccountResolution/items_1 repetitions=3 p50_ns/op=20 p95_ns/op=30 p50_B/op=40 p50_allocs/op=4 binding_loads/op=3 catalog_builds/op=1 credential_loads/op=4 sent_scan_queries/op=2
summary benchmark=BenchmarkPlain repetitions=3 p50_ns/op=200 p95_ns/op=300 p50_B/op=300 p50_allocs/op=30
summary benchmark=BenchmarkRawSourceBuilder/source_32_MiB repetitions=3 p50_ns/op=30 p95_ns/op=42 p50_B/op=40 p50_allocs/op=3 custom_count=60 source_B=33554432
summary benchmark=BenchmarkSearchBodyHeapRebound repetitions=3 p50_ns/op=200 p95_ns/op=300 p50_B/op=300 p50_allocs/op=30 retained-B/op=2.5
summary benchmark=BenchmarkSummaryRow repetitions=3 p50_ns/op=200 p95_ns/op=300 p50_B/op=300 p50_allocs/op=30 summary_row_B=80
EOF
 )"
if [[ "${ACTUAL}" != "${EXPECTED}" ]]; then
  printf 'Benchmark summary mismatch.\nExpected:\n%s\nActual:\n%s\n' "${EXPECTED}" "${ACTUAL}" >&2
  exit 1
fi

if summarize_benchmark_results "${CAPTURED_OUTPUT}" 4 >/dev/null 2>&1; then
  printf 'Benchmark summary accepted an incomplete repetition set\n' >&2
  exit 1
fi
printf '%s\n' \
  'BenchmarkMutationAccountResolution/items_1-4 1 10 ns/op 20 B/op 2 allocs/op 2 sent_scan_queries/op 3 binding_loads/op 4 credential_loads/op' \
  'BenchmarkMutationAccountResolution/items_1-4 1 20 ns/op 40 B/op 4 allocs/op 2 sent_scan_queries/op 3 binding_loads/op 4 credential_loads/op' \
  'BenchmarkMutationAccountResolution/items_1-4 1 30 ns/op 60 B/op 6 allocs/op 2 sent_scan_queries/op 3 binding_loads/op 4 credential_loads/op' \
  >"${MISSING_COUNTERS}"
if summarize_benchmark_results "${MISSING_COUNTERS}" 3 >/dev/null 2>&1; then
  printf 'Benchmark summary accepted missing account-resolution counters\n' >&2
  exit 1
fi
set -o pipefail

printf 'benchmark_summary=passed\n'

#!/usr/bin/env bash

summarize_benchmark_results() {
  local RESULT_PATH="${1:-}"
  local EXPECTED_REPETITIONS="${2:-}"

  [[ "$#" -eq 2 && -r "${RESULT_PATH}" ]] || {
    printf 'Usage: summarize_benchmark_results RESULT_PATH EXPECTED_REPETITIONS\n' >&2
    return 2
  }
  [[ "${EXPECTED_REPETITIONS}" =~ ^[1-9][0-9]*$ ]] || {
    printf 'Expected repetitions must be a positive integer: %s\n' "${EXPECTED_REPETITIONS}" >&2
    return 2
  }

  (
    set -o pipefail
    awk '
    function fail(message) {
      printf "%s\n", message > "/dev/stderr"
      failed = 1
    }
    function is_number(value) {
      return value ~ /^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$/
    }
    BEGIN { failed = 0; sample_count = 0 }
    /^Benchmark/ {
      name = $1
      sub(/-[0-9]+$/, "", name)
      ns = ""
      bytes = ""
      allocs = ""
      custom_count = 0
      for (field = 3; field <= NF; field += 2) {
        value = $field
        unit = $(field + 1)
        if (unit == "ns/op") {
          if (ns != "") fail("Benchmark " name " reports ns/op more than once")
          ns = value
        } else if (unit == "B/op") {
          if (bytes != "") fail("Benchmark " name " reports B/op more than once")
          bytes = value
        } else if (unit == "allocs/op") {
          if (allocs != "") fail("Benchmark " name " reports allocs/op more than once")
          allocs = value
        } else {
          if (unit == "" || !is_number(value)) {
            fail("Benchmark " name " reports an invalid custom metric: " value " " unit)
            continue
          }
          metric_key = NR SUBSEP unit
          if (metric_key in seen_metrics) {
            fail("Benchmark " name " reports custom metric " unit " more than once")
            continue
          }
          seen_metrics[metric_key] = 1
          custom_count++
          custom_units[custom_count] = unit
          custom_values[custom_count] = value
        }
      }
      if (!is_number(ns) || !is_number(bytes) || !is_number(allocs)) {
        fail("Benchmark " name " is missing a valid ns/op, B/op, or allocs/op value")
        next
      }
      sample_count++
      printf "%s\t\t%s\t%s\t%s\t-\n", name, ns, bytes, allocs
      for (metric = 1; metric <= custom_count; metric++) {
        printf "%s\t%s\t%s\t%s\t%s\t%s\n", name, custom_units[metric],
          ns, bytes, allocs, custom_values[metric]
      }
    }
    END {
      if (sample_count == 0) fail("No benchmark samples were recorded")
      exit failed
    }
  ' "${RESULT_PATH}" |
    LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -k6,6n |
    awk -F '\t' -v expected="${EXPECTED_REPETITIONS}" '
      function percentile_metric(values, samples, rank, ordered, sample, position, value) {
        for (sample = 1; sample <= samples; sample++) {
          value = values[sample] + 0
          for (position = sample; position > 1 && ordered[position - 1] > value; position--) {
            ordered[position] = ordered[position - 1]
          }
          ordered[position] = value
        }
        return ordered[rank]
      }
      function finish_metric(rank, item_index) {
        if (active_unit == "") return
        if (metric_count != expected) {
          printf "Benchmark %s metric %s recorded %d samples, want %d\n",
            active_name, active_unit, metric_count, expected > "/dev/stderr"
          active_valid = 0
          failed = 1
        } else {
          rank = int((metric_count + 1) / 2)
          custom_summary = custom_summary sprintf(" %s=%s", active_unit, metric_values[rank])
        }
        for (item_index = 1; item_index <= metric_count; item_index++) delete metric_values[item_index]
        metric_count = 0
      }
      function finish_benchmark(samples, p50, p95, item_index) {
        if (active_name == "") return
        samples = core_count
        if (samples != expected) {
          printf "Benchmark %s recorded %d samples, want %d\n",
            active_name, samples, expected > "/dev/stderr"
          active_valid = 0
          failed = 1
        }
        if (active_name ~ /^BenchmarkMutationAccountResolution\// &&
          (!((active_name SUBSEP "catalog_builds/op") in benchmark_metrics) ||
            !((active_name SUBSEP "sent_scan_queries/op") in benchmark_metrics) ||
            !((active_name SUBSEP "binding_loads/op") in benchmark_metrics) ||
            !((active_name SUBSEP "credential_loads/op") in benchmark_metrics))) {
          printf "Benchmark %s is missing account resolution counters\n", active_name > "/dev/stderr"
          active_valid = 0
          failed = 1
        }
        if (active_valid && samples == expected) {
          p50 = int((samples + 1) / 2)
          p95 = int((95 * samples + 99) / 100)
          printf "summary benchmark=%s repetitions=%d p50_ns/op=%.0f p95_ns/op=%.0f p50_B/op=%.0f p50_allocs/op=%.0f%s\n",
            active_name, samples, percentile_metric(ns_values, samples, p50),
            percentile_metric(ns_values, samples, p95),
            percentile_metric(byte_values, samples, p50),
            percentile_metric(alloc_values, samples, p50), custom_summary
        }
        for (item_index = 1; item_index <= core_count; item_index++) {
          delete ns_values[item_index]
          delete byte_values[item_index]
          delete alloc_values[item_index]
        }
        core_count = 0
        custom_summary = ""
      }
      BEGIN { failed = 0; active_name = ""; active_unit = "" }
      {
        if ($1 != active_name) {
          finish_metric()
          finish_benchmark()
          active_name = $1
          active_unit = ""
          active_valid = 1
        }
        unit = $2
        if (unit != active_unit) {
          finish_metric()
          active_unit = unit
        }
        if (unit == "") {
          core_count++
          ns_values[core_count] = $3
          byte_values[core_count] = $4
          alloc_values[core_count] = $5
        } else {
          metric_count++
          metric_values[metric_count] = $6
          benchmark_metrics[active_name SUBSEP unit] = 1
        }
      }
      END {
        finish_metric()
        finish_benchmark()
        exit failed
      }
    '
  )
}

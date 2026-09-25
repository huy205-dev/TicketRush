#!/usr/bin/env bash
# Summarises one request name (the k6 "name" tag) from a k6 CSV output:
# total requests, the busiest one-second window (peak RPS), the average rate
# over the seconds that had any such request, and status code counts.
#
# Usage: loadtest/peak_rps.sh loadtest/out/hold_contention.csv.gz hold
set -euo pipefail

file=${1:?usage: peak_rps.sh <k6 csv[.gz]> <name tag>}
name=${2:?usage: peak_rps.sh <k6 csv[.gz]> <name tag>}

case "$file" in
  *.gz) reader=(gzip -dc "$file") ;;
  *) reader=(cat "$file") ;;
esac

"${reader[@]}" | awk -F, -v want="$name" '
  NR == 1 {
    for (i = 1; i <= NF; i++) col[$i] = i
    if (!("metric_name" in col) || !("timestamp" in col) || !("name" in col) || !("status" in col)) {
      print "unexpected k6 CSV header" > "/dev/stderr"; exit 1
    }
    next
  }
  $col["metric_name"] == "http_reqs" && $col["name"] == want {
    total++
    per_sec[$col["timestamp"]]++
    status[$col["status"]]++
  }
  END {
    if (total == 0) { printf "no \"%s\" requests found\n", want; exit 1 }
    peak = 0; secs = 0
    for (s in per_sec) { secs++; if (per_sec[s] > peak) { peak = per_sec[s]; peak_at = s } }
    printf "%s: %d requests over %d active seconds\n", want, total, secs
    printf "  peak: %d req/s (at unix %s)\n", peak, peak_at
    printf "  mean over active seconds: %.0f req/s\n", total / secs
    printf "  status:"
    for (c in status) printf " %s=%d", c, status[c]
    printf "\n"
  }'

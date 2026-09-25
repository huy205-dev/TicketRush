#!/usr/bin/env bash
# Turns the first failure lines of a `go test` log into GitHub error
# annotations. Annotations are readable through the public API, unlike job
# logs, which need admin rights on the repository.
set -euo pipefail
log=${1:?usage: report-test-failures.sh <go test output>}
grep -E -e '^\s*--- FAIL' -e '^FAIL\s' -e '_test\.go:[0-9]+:' -e '^panic:' \
  -e '^(testenv|testdb|testredis|testkafka):' "$log" | head -n 10 |
  while IFS= read -r line; do
    printf '::error::%s\n' "$line"
  done

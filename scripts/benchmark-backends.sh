#!/usr/bin/env bash
set -euo pipefail

selection="${1:-local}"
case "$selection" in
  local)
    backends=(eqmem eqsqlite eqgrpc)
    ;;
  all)
    backends=(eqmem eqsqlite eqgrpc eqredis eqpg)
    ;;
  *)
    backends=("$@")
    ;;
esac

benchtime="${ENTROQ_BENCHTIME:-1s}"
count="${ENTROQ_BENCHCOUNT:-3}"

for backend in "${backends[@]}"; do
  case "$backend" in
    eqmem|eqsqlite|eqgrpc|eqredis|eqpg) ;;
    *)
      echo "unknown backend: $backend" >&2
      exit 2
      ;;
  esac
  go test "./pkg/backend/$backend" \
    -run '^$' \
    -bench '^BenchmarkBackend$' \
    -benchmem \
    -benchtime "$benchtime" \
    -count "$count"
done

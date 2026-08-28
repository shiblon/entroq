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
done

for ((sample = 0; sample < count; sample++)); do
  for ((backend_offset = 0; backend_offset < ${#backends[@]}; backend_offset++)); do
    backend="${backends[(sample + backend_offset) % ${#backends[@]}]}"
    go test "./pkg/backend/$backend" \
      -run '^$' \
      -bench '^BenchmarkBackend$' \
      -benchmem \
      -benchtime "$benchtime" \
      -count 1
  done
done

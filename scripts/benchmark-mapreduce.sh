#!/usr/bin/env bash
set -euo pipefail

selection="${1:-local}"
case "$selection" in
  local)
    profiles=(grpc-memory grpc-journal grpc-sqlite)
    ;;
  all)
    profiles=(grpc-memory grpc-journal grpc-sqlite grpc-redis grpc-postgres)
    ;;
  *)
    profiles=("$@")
    ;;
esac

benchtime="${ENTROQ_LOAD_BENCHTIME:-3x}"
count="${ENTROQ_LOAD_BENCHCOUNT:-3}"
modes=(Baseline Stats250ms Stats5s)

for ((sample = 0; sample < count; sample++)); do
  for ((profile_offset = 0; profile_offset < ${#profiles[@]}; profile_offset++)); do
    profile="${profiles[(sample + profile_offset) % ${#profiles[@]}]}"
    case "$profile" in
      grpc-memory|grpc-journal)
        package=eqgrpc
        ;;
      grpc-sqlite)
        package=eqsqlite
        ;;
      grpc-redis)
        package=eqredis
        ;;
      grpc-postgres)
        package=eqpg
        ;;
      *)
        echo "unknown MapReduce profile: $profile" >&2
        exit 2
        ;;
    esac
    for ((mode_offset = 0; mode_offset < ${#modes[@]}; mode_offset++)); do
      mode="${modes[(sample + mode_offset) % ${#modes[@]}]}"
      go test "./pkg/backend/$package" \
        -run '^$' \
        -bench "^BenchmarkMapReduceLoad$/^${profile}$/^${mode}$" \
        -benchmem \
        -benchtime "$benchtime" \
        -count 1
    done
  done
done

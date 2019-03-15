#!/bin/bash

cd "$(dirname "$0")"

echo "$@"

case "$1" in
  pg)
    shift
    exec eqpgsvc "$@"
    ;;
  mem | '')
    shift
    exec eqmemsvc "$@"
    ;;
  eqc | '')
    shift
    exec eqc "$@"
    ;;
  -*)
    exec eqmemsvc "$@"
    ;;
  *)
    echo "Unknown command '$1'"
    exit - 1
    ;;
esac

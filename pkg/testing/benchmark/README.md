# Backend benchmarks

Recorded runs:

- [2026-08-28 backend comparison](results-2026-08-28.md)

The common backend suite measures the same public EntroQ operations and seeds
the same data through the public API for every storage backend. Setup is outside
the benchmark timer, each scenario gets isolated data, and steady-state write
scenarios replace every task they consume.

The suite reports both direct backend calls and calls through an in-process
gRPC service for every backend. The gRPC variants include protobuf conversion,
the gRPC client and server, and the service layer, but use `bufconn` rather than
a real network so network RTT does not obscure backend differences.

The suite compares each backend's ordinary test configuration, not identical
durability conditions. SQLite uses `synchronous=FULL`; journaled eqmem writes
its WAL; PostgreSQL uses the testcontainer's defaults; and Redis's test
container does not enable AOF persistence. Interpret the results as
implementation profiles, not a durability-adjusted database ranking.

Run the local backends (memory-only eqmem, journaled eqmem, and SQLite, both
directly and through gRPC):

```sh
./scripts/benchmark-backends.sh
```

Run all storage backends, including Redis and PostgreSQL testcontainers:

```sh
./scripts/benchmark-backends.sh all
```

Docker is required for the Redis and PostgreSQL packages. Runs are sequential so
the service-backed packages do not compete with each other. Adjust duration and
sample count with `ENTROQ_BENCHTIME` and `ENTROQ_BENCHCOUNT`:

```sh
ENTROQ_BENCHTIME=3s ENTROQ_BENCHCOUNT=5 ./scripts/benchmark-backends.sh all
```

Specific backends may be named explicitly:

```sh
./scripts/benchmark-backends.sh eqmem eqsqlite
```

The `grpc-*` variants measure protobuf, RPC, and service overhead through a
buffered in-process transport. They do not include real network latency.

The suite covers empty `TryClaim`, serial and parallel claim/complete worker
cycles, one- and 32-task atomic handoffs, and queue-stat/task-list reads at
depths of 1,000 and 10,000. Benchmark names include the backend and scenario,
so repeated output can be compared with tools such as `benchstat`. Claim/complete
scenarios also report `tryclaims/op`; values above one expose transient misses
and retry pressure under contention.

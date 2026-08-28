# Backend benchmarks

Recorded runs:

- [2026-08-28 backend comparison](results-2026-08-28.md)
- [2026-08-28 MapReduce load and stats comparison](mapreduce-results-2026-08-28.md)

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
its WAL; PostgreSQL 17 uses the testcontainer's defaults; and Redis's test
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
the service-backed packages do not compete with each other. Each sample runs in
a fresh Go process, and backend order rotates between samples. Adjust duration
and sample count with `ENTROQ_BENCHTIME` and `ENTROQ_BENCHCOUNT`:

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

## MapReduce load and stats polling

The MapReduce suite runs a deterministic 1,000-document word-count pipeline
through gRPC with 16 mappers and four reducers. It compares the unobserved
baseline with `QueueStats` polling every 250 milliseconds and every five
seconds. Each sampler permits only one call in flight, fails on polling errors,
and reports stats latency plus queue and claim high-water marks. Final results
are verified outside the benchmark timer.

The default runs three complete jobs per sample and collects three independent
samples. Every backend/mode/sample combination gets a fresh Go process, which
prevents earlier modes from biasing later ones through heap pressure, warm
containers, or database caches. Mode and backend order rotate between samples
to balance machine-time drift:

```sh
./scripts/benchmark-mapreduce.sh
./scripts/benchmark-mapreduce.sh all
```

Adjust the jobs per sample and independent sample count with
`ENTROQ_LOAD_BENCHTIME` and `ENTROQ_LOAD_BENCHCOUNT`. Profiles may also be named
explicitly, for example `./scripts/benchmark-mapreduce.sh grpc-redis
grpc-postgres`. Runs are sequential, and Redis and PostgreSQL require Docker.

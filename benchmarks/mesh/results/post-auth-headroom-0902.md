# Mesh latency with queue headroom after the authentication boundary shift

Measured 2026-09-02 from `feat/mesh-benchmark-post-auth` at revisions
`3bc9460` through `4d821c4`, based on `develop` at `035b537`.

## Question and method

This run asks how EntroQ behaves as a transport when work is nominally
immediate and queues have headroom. It is not a saturation benchmark.

- 1 KiB HTTP payload, concurrency 4, fixed 10 requests/second.
- Three 30-second samples after a 3-second warm-up.
- Four k3d agents on one non-burstable `m6i.2xlarge` EC2 host.
- Memory, Redis 7 with persistence disabled, and PostgreSQL 17 with container
  defaults each ran in fresh clusters on that same host.
- EntroQ and OPA each had a 2-CPU limit.
- The security-equivalent direct baseline verifies the same JWT and makes one
  exact per-service OPA decision. Raw direct HTTP is an unauthenticated floor.
- One mesh hop makes five OPA decisions per request; two hops make ten.

The host began at 9% root-disk use with `/tmp` at 1% and about 30 GiB memory
available. It ended at 18% root-disk use with `/tmp` still at 1% and about
29 GiB available. All comparisons below therefore share one clean physical
host. The k3d agents are still containers, not a multi-machine production
deployment.

## Results

All accepted runs sustained 10 requests/second with no request failures,
invalid responses, or metric scrape errors.

### No OPA: transport and backend floor

| Backend | One-hop p50 / p95 / p99 | Two-hop p50 / p95 / p99 |
|---|---:|---:|
| memory | 2.33 / 2.75 / 3.04 ms | 4.30 / 5.21 / 5.95 ms |
| Redis 7, persistence disabled | 3.78 / 4.55 / 5.06 ms | 7.00 / 8.65 / 9.36 ms |
| PostgreSQL 17, container defaults | 15.73 / 17.46 / 18.38 ms | 27.52 / 31.34 / 33.50 ms |

Redis adds 1.45 ms one-hop and 2.70 ms two-hop to the memory p50. PostgreSQL
adds 13.40 and 23.22 ms. At this offered rate, the external Redis operation is
small relative to eqlink, gRPC, protobuf, and HTTP orchestration; PostgreSQL's
transactional path is the material backend cost.

### Full policy and security-equivalent direct baseline

| Backend | Authorized direct p50 / p95 / p99 | One-hop p50 / p95 / p99 | Two-hop p50 / p95 / p99 |
|---|---:|---:|---:|
| memory | 1.67 / 1.95 / 2.36 ms | 4.84 / 6.18 / 7.29 ms | 9.03 / 11.16 / 12.13 ms |
| Redis 7 | 1.68 / 1.98 / 2.39 ms | 6.28 / 8.05 / 9.20 ms | 11.74 / 13.91 / 14.91 ms |
| PostgreSQL 17 | 1.64 / 1.95 / 2.19 ms | 13.60 / 15.25 / 16.34 ms | 25.52 / 28.94 / 32.55 ms |

Subtracting the paired security-equivalent baseline gives the cost of mesh
transport plus the additional four or nine OPA decisions:

| Backend | Added one-hop p50 / p95 / p99 | Added two-hop p50 / p95 / p99 |
|---|---:|---:|
| memory | 3.17 / 4.23 / 4.93 ms | 7.36 / 9.21 / 9.77 ms |
| Redis 7 | 4.60 / 6.07 / 6.81 ms | 10.06 / 11.93 / 12.52 ms |
| PostgreSQL 17 | 11.96 / 13.30 / 14.15 ms | 23.88 / 26.99 / 30.36 ms |

The direct baselines were stable across backend runs: 0.41--0.42 ms raw and
1.64--1.68 ms authorized. Backend differences are therefore visible without
the cross-host ambiguity in the earlier measurements.

## Where the time goes

The memory A/B included an allow-all OPA policy so JWT authentication, OPA
HTTP, JSON, and scheduling stayed present while Rego policy work was minimal.

| Path | Allow-all p50 | Full-policy p50 | Full-minus-allow-all |
|---|---:|---:|---:|
| authorized direct, 1 decision | 1.29 ms | 1.67 ms | 0.38 ms |
| one hop, 5 decisions | 3.76 ms | 4.84 ms | 1.08 ms |
| two hops, 10 decisions | 6.63 ms | 9.03 ms | 2.40 ms |

These separate fresh clusters still have run-to-run noise, so subtraction is
indicative rather than a direct timer. It suggests that the current small full
policy adds roughly 0.2--0.4 ms per decision over the trivial policy. The
verified-principal boundary has removed JWT parsing, signature verification,
and JWKS work from Rego.

The reproducible post-boundary prepared-query benchmark exposes the scaling
risk more clearly:

| Queue policies | Current list-shaped policy | Precomputed identity grants |
|---:|---:|---:|
| 2 | 0.214 ms | 0.132 ms |
| 10 | 0.348 ms | 0.132 ms |
| 100 | 1.895 ms | 0.134 ms |
| 1000 | 16.052 ms | 0.138 ms |

At 1,000 policies the current decision is about 116 times slower and allocates
about 5.9 MiB versus 57 KiB per evaluation. The indexed prototype stays nearly
flat because unrelated policy entries are not scanned.

CPU was not the ceiling. Across measured mesh windows, EntroQ peaked at
61--141 millicores and OPA at 84--85 millicores against 2,000-millicore limits.
Redis peaked at 28 millicores and 8 MiB RSS; PostgreSQL peaked at 261
millicores and 48 MiB RSS. The earlier `52m`/`55m` Redis figures were CPU
millicores, not memory measurements.

## Task granularity guidance

Using the paired full-policy increments, 500 ms of useful work incurs:

| Backend | One-hop p50 / p95 overhead | Two-hop p50 / p95 overhead |
|---|---:|---:|
| memory | 0.6% / 0.8% | 1.5% / 1.8% |
| Redis 7 | 0.9% / 1.2% | 2.0% / 2.4% |
| PostgreSQL 17 | 2.4% / 2.7% | 4.8% / 5.4% |

For one or two hops, “half a second of useful work will not feel slower” is a
sound human rule of thumb on this topology. The conservative PostgreSQL p50
thresholds are about 120 ms one-hop and 240 ms two-hop for 10% overhead, or
240 and 480 ms for 5%. Sequential chains add their hop costs.

This is not a universal direct-versus-mesh cutoff. Reliability, replay,
durable state transitions, independent scaling, and failure isolation can be
worth materially more than the latency budget. For tiny synchronous work,
direct networking remains the honest default; the operator supports a hybrid
topology by routing selected localhost destinations through eqlink and letting
other traffic pass directly.

## Optimization order

1. **Precompute per-principal grants in the Kubernetes operator.** Compile the
   finite service-account mesh graph into OPA data keyed by verified subject
   instead of scanning every queue policy for every operation. This keeps the
   decision count unchanged but removes the demonstrated policy-size slope.
2. **Reduce the five decisions per hop with explicit, scoped capabilities.**
   A claim can return lease-bound authority for a later modification. Optional
   insertion hints can describe an upper bound; actual operations must be a
   subset, and uncovered operations still require normal authorization.
3. **Then reduce OPA transport overhead.** Eliminating a remote decision
   multiplies across five or ten calls more effectively than shaving one call.
4. **Profile raw transport on production backends before changing it.** Redis
   adds little under headroom, while PostgreSQL backend work is already visible.

## Invalidated runs and harness repairs

- The earlier local memory, Redis, and PostgreSQL measurements are superseded.
  The host had recently been at 98--100% root-disk use and its memory-backed
  `/tmp` had been full. A local PostgreSQL retry also recorded
  `FreeDiskSpaceFailed` and `ImageGCFailed`.
- One clean-host no-OPA run lost its kubeconfig from `/tmp` and was rejected.
  Commit `a273f58` keeps it with the ignored run artifacts instead.
- A Redis smoke saw a metrics Service before its endpoint route was usable;
  a PostgreSQL smoke similarly saw initial Service connection failures.
  Commits `7f4a0e5` and `4d821c4` warm smoke paths and allow kube-proxy routes
  to settle without relaxing strict warm-up, request, or metric validation.
- Fresh k3d clusters recorded transient startup warnings while flannel,
  metrics-server, and backend readiness converged. Accepted measurements began
  only after rollout, settling, and smoke checks; there were no disk-pressure,
  image-GC, eviction, restart, request, validation, or metric-scrape failures.

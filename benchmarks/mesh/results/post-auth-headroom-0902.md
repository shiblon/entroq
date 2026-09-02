# Mesh latency with queue headroom after the authentication boundary shift

Measured 2026-09-01/02 from `feat/mesh-benchmark-post-auth` at commits
`695872d` and `18064e6`, based on `develop` at `035b537`.

## Question and method

This run asks how EntroQ behaves as a transport when work is nominally
immediate and queues have headroom. It is not a saturation benchmark.

- 1 KiB HTTP payload, concurrency 4, fixed 10 requests/second.
- Three 30-second samples after a 3-second warm-up.
- Four k3d agents, with the storage container pinned to the control-plane
  node for Redis and PostgreSQL.
- EntroQ and OPA each had a 2-CPU limit.
- The security-equivalent direct baseline verifies the same JWT and makes one
  per-service OPA decision. Raw direct HTTP remains an explicitly
  unauthenticated lower bound.
- One mesh hop makes five OPA decisions per request; two hops make ten.

Memory and Redis ran on the same local host. PostgreSQL ran on a clean
`m6i.2xlarge` EC2 host with a 100 GB gp3 volume because the local Docker host
entered k3s image garbage collection. PostgreSQL absolute latency therefore
must not be ranked directly against the local-host rows. Within each row,
paths share a host and fixed offered rate.

## Results

All accepted runs sustained 10 requests/second with no failures or invalid
responses. A timer-edge sample completed 299 rather than 300 requests in one
memory mode without lowering the reported 10.0 requests/second.

### No OPA: transport and backend floor

| Backend | One-hop p50 / p95 / p99 | Two-hop p50 / p95 / p99 |
|---|---:|---:|
| memory, local | 6.98 / 15.56 / 21.47 ms | 11.93 / 29.67 / 37.25 ms |
| Redis 7, local, persistence disabled | 7.67 / 15.14 / 18.42 ms | 14.79 / 27.22 / 33.66 ms |
| PostgreSQL 17, EC2, container defaults | 13.68 / 15.96 / 16.72 ms | 25.33 / 30.23 / 32.19 ms |

Redis adds only 0.69 ms one-hop and 2.86 ms two-hop to the local memory p50.
At this offered rate, eqlink, gRPC, protobuf work, queue operations, and HTTP
round trips dominate the memory-versus-Redis difference.

### Full policy and security-equivalent direct baseline

| Backend | Authorized direct p50 / p95 / p99 | One-hop p50 / p95 / p99 | Two-hop p50 / p95 / p99 |
|---|---:|---:|---:|
| memory, local | 3.23 / 7.32 / 11.26 ms | 14.13 / 30.10 / 44.11 ms | 27.83 / 61.57 / 75.40 ms |
| Redis 7, local | 2.86 / 4.81 / 7.28 ms | 14.10 / 27.89 / 33.87 ms | 26.42 / 44.64 / 54.30 ms |
| PostgreSQL 17, EC2 | 1.10 / 1.47 / 1.77 ms | 12.34 / 14.30 / 15.31 ms | 24.75 / 28.28 / 31.68 ms |

Subtracting the paired security-equivalent baseline gives the cost of mesh
transport plus the additional four or nine OPA decisions:

| Backend | Added one-hop p50 / p95 / p99 | Added two-hop p50 / p95 / p99 |
|---|---:|---:|
| memory, local | 10.90 / 22.78 / 32.85 ms | 24.60 / 54.25 / 64.14 ms |
| Redis 7, local | 11.24 / 23.08 / 26.59 ms | 23.56 / 39.83 / 47.02 ms |
| PostgreSQL 17, EC2 | 11.24 / 12.83 / 13.54 ms | 23.65 / 26.81 / 29.91 ms |

The near-identical p50 increments are more informative than the absolute
cross-host values: roughly 11 ms for one hop and 24 ms for two hops under
headroom. Separate no-OPA and full-policy clusters have enough run-to-run
noise that their medians must not be naively subtracted. PostgreSQL full-policy
medians happened to be slightly lower than its no-OPA medians; authorization
is not free.

## Where the time goes

The memory A/B included an allow-all OPA policy so JWT authentication, OPA
HTTP, JSON, and scheduling stayed present while Rego policy work was minimal.

| Path | Allow-all p50 | Full-policy p50 | Full-minus-allow-all |
|---|---:|---:|---:|
| authorized direct, 1 decision | 2.79 ms | 3.23 ms | 0.44 ms |
| one hop, 5 decisions | 10.45 ms | 14.13 ms | 3.68 ms |
| two hops, 10 decisions | 20.85 ms | 27.83 ms | 6.98 ms |

Median client-observed OPA time was 1.62/1.69 ms per decision for allow-all
one/two-hop traffic and 2.99/2.91 ms for full policy. OPA's own median handler
time was 0.53/0.52 ms allow-all and 1.61/1.52 ms full. The full policy therefore
adds about 1.0 ms of server evaluation per decision, while HTTP transport,
JSON, scheduling, and client-side waiting account for roughly another
1.1--1.4 ms per decision.

The verified-principal boundary shift removed repeated JWT signature checks
from Rego. Compared with the pre-shift profile, the full-minus-allow-all p50
gap fell from 1.34 to 0.44 ms for direct, 4.76 to 3.68 ms for one hop, and
17.95 to 6.98 ms for two hops. Absolute one-hop latency did not improve on the
noisy local host, but the policy-only multiplier did, especially at ten
decisions per request.

CPU was not the ceiling. Coarse maxima were 64--99 millicores for EntroQ and
93--97 millicores for OPA on the local accepted runs, against 2000-millicore
limits. The clean EC2 PostgreSQL run peaked at 127 millicores for EntroQ, 144
for OPA, and 169 for PostgreSQL. PostgreSQL no-OPA peaked at 136 millicores for
EntroQ and 289 for PostgreSQL.

## Task granularity guidance

Using the approximately 11/24 ms p50 increment:

- One-hop work around 110 ms makes mesh overhead about 10% of useful work;
  around 220 ms makes it about 5%.
- Two-hop work around 240 ms makes mesh overhead about 10%; around 480 ms
  makes it about 5%.
- A conservative local-host p95 budget is about 230 ms of useful work for one
  hop and 540 ms for two hops at the 10% line.

This is not a universal direct-versus-mesh cutoff. Reliability, replay,
durable state transitions, independent scaling, and failure isolation can be
worth materially more than the latency budget. For tiny synchronous work
below a few tens of milliseconds, direct networking is usually the honest
default; the operator already supports a hybrid topology by routing selected
localhost destinations through eqlink and letting other traffic pass through.

## Optimization order

1. **Precompute per-principal grants in the operator.** A pre-boundary Rego
   microprofile grew from 0.882 ms at two policy entries to 66.174 ms at 1000,
   while precomputed identity grants stayed near 0.2 ms. This is the largest
   scaling opportunity and also simplifies user-customized Rego.
2. **Reduce the five decisions per hop with explicit, scoped capabilities.**
   Authentication should remain front-loaded, and authorization context must
   remain typed and auditable. A narrowly scoped response capability is a
   candidate; ambient identity or ambient authority is not.
3. **Then reduce OPA transport overhead.** The client/server gap is real, but
   eliminating decisions multiplies the benefit more effectively than shaving
   one HTTP call while retaining ten calls per request.
4. **Optimize raw EntroQ transport after profiling a production backend.** The
   no-OPA floor is measurable, but at 10 requests/second neither EntroQ nor the
   databases approached a CPU limit.

## Invalidated and repaired runs

- A local PostgreSQL/full run had one 10-second warm-up timeout. A retained
  retry exposed `FreeDiskSpaceFailed` and `ImageGCFailed` on every k3d node at
  92--93% host disk usage. Those local PostgreSQL results are excluded.
- The clean EC2 run stayed at 9--12% disk usage and recorded only
  `NodeHasNoDiskPressure`; all PostgreSQL samples passed.
- The first SSM smoke completed every workload but report generation failed
  because SSM supplied neither `HOME` nor `GOPATH`. Commit `18064e6` gives the
  reporter explicit `GOCACHE` and `GOMODCACHE` paths. Regenerating the report
  over the preserved smoke data then passed.
- The temporary EC2 instance was terminated and its delete-on-termination
  volume was confirmed absent.

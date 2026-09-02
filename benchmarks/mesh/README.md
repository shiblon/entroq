# Local Kubernetes mesh benchmark

This harness compares matched requests through raw Kubernetes HTTP, an
OPA-authorized direct proxy, one EntroQ mesh hop, and two EntroQ mesh hops. It is
a small deployment-shaped benchmark: memory backend, a 1 KiB echo request, and
three repeated samples per path by default.

```text
raw direct:         load job ────────HTTP────────────────────> leaf handler
authorized direct:  load job -> OPA-gated proxy ─HTTP────> leaf handler
1 hop:              load job -> gateway sender -> EntroQ + OPA
                    -> leaf receiver -> leaf handler
2 hops:             load job -> gateway sender -> EntroQ + OPA -> relay receiver
                    -> relay sender -> EntroQ + OPA -> leaf receiver -> leaf handler
```

The load generator runs inside the cluster, so host ingress and port forwarding
are outside the timed path. Gateway, relay, leaf, and load pods use required pod
anti-affinity across four k3d agents. The agents are containers on one physical
host; report these results as **local-cluster mesh capacity**, not as a
multi-machine production estimate.

## Run it

Prerequisites: Docker, k3d, kubectl, Helm, Go, and enough capacity for one k3s
server plus four agents.

```bash
./scripts/mesh-benchmark.sh
```

The script creates a uniquely named, pinned k3d cluster, builds and imports
local EntroQ images, installs the chart and CRDs, proves the OPA-authorized mesh
path, runs fresh in-cluster Jobs for every sample, writes a report, and deletes
the cluster. Set `KEEP_CLUSTER=1` to retain it for inspection.

Useful overrides:

```bash
SAMPLES=5 DURATION=30s CONCURRENCY=16 ./scripts/mesh-benchmark.sh
PAYLOAD_BYTES=65536 ./scripts/mesh-benchmark.sh
TARGET_RPS=10 CONCURRENCY=4 DURATION=30s ./scripts/mesh-benchmark.sh
AUTHZ_STRATEGY=none ./scripts/mesh-benchmark.sh
OPA_POLICY_MODE=allow-all ./scripts/mesh-benchmark.sh
BACKEND=redis ./scripts/mesh-benchmark.sh
BACKEND=postgres ./scripts/mesh-benchmark.sh
```

`K3S_IMAGE`, `CLUSTER_NAME`, `RESULT_DIR`, `WARMUP`, `REQUEST_TIMEOUT`, and
`METRIC_INTERVAL` are also configurable. `ENTROQ_CPU_LIMIT` and `OPA_CPU_LIMIT`
default to the chart limits of `500m` and `250m`; changing either creates a
distinct capacity configuration. The default k3s image is pinned in the script;
changing it creates a distinct benchmark configuration.

`ENTROQ_SOURCE_ROOT` can point at another EntroQ worktree. The harness builds
the server and operator images and installs the chart from that tree, while
retaining this tree's instrumented eqlink and load-generator sources. This is
useful for testing an isolated implementation branch without copying benchmark
fixtures into it.

`TARGET_RPS` changes the harness from an unpaced capacity test to a fixed-rate
latency test. The rate is shared across all load workers, and request timing
starts only after a pacing tick. Use a rate comfortably below the slowest
measured mode; a sample fails if achieved throughput is below 90% of the target.

`AUTHZ_STRATEGY=none` removes the OPA container and OPA-only resources entirely;
this isolates queue and gRPC orchestration from authorization. With the default
`AUTHZ_STRATEGY=opahttp`, `OPA_POLICY_MODE=full` runs the JWT and mesh-policy
decision, while `OPA_POLICY_MODE=allow-all` preserves the same number of OPA
round trips with a trivial decision. Together these form a diagnostic ladder:
no OPA, trivial OPA, and full OPA.

`BACKEND` selects `memory`, `redis`, or `postgres`. Redis runs with RDB
snapshots and AOF disabled. PostgreSQL uses version 17 container defaults on an
ephemeral volume. Both external stores run on the k3d control-plane node so
their network hop is stable and separate from the four benchmark agents.

For a harness-only rerun, reuse previously built images by setting both their
tag and `BUILD_IMAGES=0`:

```bash
IMAGE_TAG=meshbench-20260831T120000Z BUILD_IMAGES=0 ./scripts/mesh-benchmark.sh
```

## Results and fairness boundary

Results land under `benchmarks/mesh/results/<run-id>/` and include:

- one raw JSON file per fresh load-generator process;
- periodic raw Prometheus snapshots from eqlink, the EntroQ backend, and OPA;
- periodic per-container CPU/memory snapshots when the k3s metrics server is available;
- node, pod, event, and tool-version context;
- `summary.md` with per-sample and median/range throughput and latency.

Warm-up is outside the timed interval. Mode order rotates across samples to
balance machine-time drift, every response body is validated, and any request or
metric-scrape error fails the run. Scrapes canceled solely by sampler shutdown
are discarded before a final snapshot is taken. The report also fails unless
source-specific sender and receiver counters are positive for every measured
hop.

The raw direct mode is an unauthenticated transport lower bound, not a
security-equivalent baseline. The authorized direct proxy verifies the gateway
service-account JWT with the same bounded token and JWKS caches as the EntroQ
service, then asks OPA for the exact leaf inbox `INSERT` permission. The
full-policy run also proves that an unknown service is denied without reaching
the leaf. This matches authentication and policy semantics, but not topology:
the proxy reaches OPA through a cluster Service while EntroQ reaches its OPA
sidecar over localhost. It also intentionally makes one decision per request,
where the current queue protocol makes five decisions per hop. The report uses
OPA request counters to show that multiplier and pairs ratios by sample.

The default backend is intentionally `memory`: data is lost on restart. Redis
and PostgreSQL modes compare backend overhead, not equivalent durability;
their persistence settings are reported above. No CPU limits are applied to
the benchmark pods.

The gRPC server's minimum permitted transport keepalive matches the client's
30-second keepalive. This is separate from EntroQ worker claim polling, which is
a server-side fallback and is not the source of keepalive disconnects.

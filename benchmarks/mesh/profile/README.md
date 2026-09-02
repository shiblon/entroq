# Mesh authorization profiles

These fixtures isolate the queue-policy portion of the Kubernetes mesh Rego
from JWT verification. They use the same queue and identity shapes as the
fixed-rate mesh benchmark, but replace `data.entroq.user.name` with a static,
already-authenticated subject.

Run a representative operation with the OPA CLI:

```bash
opa bench \
  --data ../../../pkg/authz/opadata/conf/core/entroq/authz/core-entroq-authz.rego \
  --data ../../../pkg/authz/opadata/conf/core/entroq/queues/core-entroq-queues.rego \
  --data ../../../pkg/authz/opadata/conf/core/entroq/namespaces/core-entroq-namespaces.rego \
  --data ../../../pkg/authz/opadata/conf/providers/k8s/permissions/k8s-entroq-permissions.rego \
  --data data.json \
  --data static-user.rego \
  --input insert.json \
  'data.entroq.authz'
```

Replace `insert.json` with `claim.json`, `delete-insert.json`,
`response-claim.json`, or `response-delete.json` to profile the other decisions
made by one eqlink request.

The Go benchmarks exercise prepared queries in the same OPA library version as
EntroQ. `BenchmarkK8sPolicy` uses a real RSA-signed JWT and a warm local JWKS
cache. The scale benchmarks compare the current list-shaped mesh policy with a
prototype identity-keyed grants document:

```bash
go test ./pkg/authz/opadata -run '^$' -bench '^BenchmarkK8sPolicy$' -benchmem -count=5
go test ./pkg/authz/opadata -run '^$' -bench '^BenchmarkK8sPolicyScale$' -benchmem -count=3
go test ./pkg/authz/opadata -run '^$' -bench '^BenchmarkK8sPrecomputedPolicyScale$' -benchmem -count=3
```

These are diagnostic benchmarks, not production throughput claims. The static
identity benchmarks deliberately exclude token parsing, signature verification,
JWKS transport, and the HTTP hop between EntroQ and its OPA sidecar.

# eqk8s — EntroQ Kubernetes Operator

eqk8s is a Kubernetes operator that manages OPA authorization policies for
EntroQ deployments. It watches two custom resource types — `EntroQQueue` and
`EntroQIdentity` — and continuously pushes a mesh authorization document to OPA
so the EntroQ server can enforce fine-grained access control without restarts or
manual data API calls.

See `pkg/authz/opadata/OPA_AUTHZ.md` in the main repository for a detailed
explanation of the OPA policy design, OIDC provider configuration, and how the
authorization layer integrates with the EntroQ gRPC and HTTP servers.

---

## How It Works

```
EntroQIdentity CRDs        EntroQQueue CRDs
  (SA → labels)         (queue + namespace policies)
        │                        │
        └────────────┬───────────┘
                     ▼
              MeshReconciler
        (rebuilds on every change)
                     │
          ┌──────────┴──────────┐
          ▼                     ▼
   OPA data API          ConfigMap
  PUT /v1/data/mesh    entroq-mesh
  (live updates)      (startup volume)
                             │
                    OPA sidecar reads on boot
                             │
                      EntroQ server
              checks data.entroq.authz.allow
              for every gRPC / HTTP call
```

Every time an `EntroQQueue` or `EntroQIdentity` resource is created, updated, or
deleted anywhere in the cluster, the reconciler:

1. Lists all `EntroQQueue` and `EntroQIdentity` resources across all namespaces.
2. Builds an `OPAMesh` document from them.
3. `PUT`s it to `http://localhost:8181/v1/data/mesh` (OPA's data API).
4. Writes it to the `entroq-mesh` ConfigMap so OPA can load it on startup via a
   volume mount.

---

## CRD Types

### `EntroQIdentity` — service account labels

Maps Kubernetes service accounts to the mesh label claims they present for
authorization decisions. Multiple service accounts can share an identity
resource, and multiple `EntroQIdentity` resources can coexist in the same
namespace.

```yaml
apiVersion: entroq.entroq.io/v1alpha1
kind: EntroQIdentity
metadata:
  name: payments-identities
  namespace: payments
spec:
  identities:
  - serviceAccount: svc-a
    labels:
      group: frontend
      team: payments
  - serviceAccount: svc-b
    labels:
      group: backend
```

The operator constructs identity keys of the form
`system:serviceaccount:<namespace>:<serviceAccount>` from the resource namespace
and the `serviceAccount` field.

---

### `EntroQQueue` — queue and namespace access policies

Declares which callers may access which task queues and doc namespaces. Each
pattern lists the label matchers that authorize access: **AND** semantics within
one `labels` entry, **OR** semantics across multiple entries.

```yaml
apiVersion: entroq.entroq.io/v1alpha1
kind: EntroQQueue
metadata:
  name: svc-b-policy
  namespace: payments
spec:
  # queues grants task-queue access.
  queues:
  - pattern: /payments/svc-b/inbox
    matchType: Exact          # or Prefix
    allowedCallers:
    - labels:
        group: frontend       # svc-a satisfies this (OR)
    - labels:
        group: internal-tools # any internal-tools service satisfies this (OR)
        team: payments        # ... but must also have team=payments (AND)

  # namespaces grants doc-namespace access. Omit if the service doesn't use
  # the EntroQ document store.
  namespaces:
  - pattern: /payments/svc-b/
    matchType: Prefix
    allowedCallers:
    - labels:
        group: frontend
    - labels:
        group: internal-tools
        team: payments
```

`matchType` is `Exact` (default) or `Prefix`.

A single `EntroQQueue` resource can carry both `queues` and `namespaces`
sections, which is convenient when the same set of callers needs both task-queue
and document-store access for a service.

---

## Authorization Logic

The k8s OPA provider (loaded from `conf/providers/k8s/`) derives allowed access
from three sources:

### 1. Auto-grant (always on, no CRD needed)

Every service account automatically receives `ALL` access to its own queue
prefix, derived from its subject claim:

```
system:serviceaccount:payments:svc-a  →  ALL on /payments/svc-a/
```

This covers the service's inbox, response queues, and GC queues without any
operator configuration.

### 2. Mesh grants (from CRDs)

For cross-service calls, the caller's labels (from `EntroQIdentity`) are checked
against each queue or namespace policy (from `EntroQQueue`). If the caller's
labels satisfy any `allowedCallers` entry in a policy, access is granted.

If `data.mesh.initialized` is false or absent — meaning OPA just restarted and
the operator hasn't reconciled yet — mesh grants are suppressed. Auto-grants
still fire. This prevents a temporarily uninitialized OPA from becoming an open
gateway.

### 3. Response queue grant (automatic)

Any caller permitted on `X/inbox` also receives `ALL` on `X/response/`.
Response queues are ephemeral per-request queues created by the eqlink sidecar;
callers need prefix access to claim replies without explicit policy entries for
every nonce.

---

## The Mesh Document

You can inspect the current mesh document pushed to OPA at any time:

```bash
curl http://localhost:8181/v1/data/mesh | jq .
```

Example output (with namespace policies added):

```json
{
  "initialized": true,
  "identities": {
    "system:serviceaccount:payments:svc-a": {
      "labels": {"group": "frontend", "team": "payments"}
    }
  },
  "queues": [
    {
      "pattern": "/payments/svc-b/inbox",
      "matchType": "Exact",
      "allowedCallers": [{"group": "frontend"}]
    }
  ],
  "namespaces": [
    {
      "pattern": "/payments/svc-b/",
      "matchType": "Prefix",
      "allowedCallers": [{"group": "frontend"}]
    }
  ]
}
```

`namespaces` is populated from the `spec.namespaces` field of `EntroQQueue`
resources. It is omitted (empty array) when no namespace policies are defined.

---

## Namespace Access (Doc Store)

If your services use the EntroQ document store, you can grant namespace-level
access using the `namespaces` field in `EntroQQueue`. Without it, the k8s
provider always returns an empty namespace grant set — services can operate on
task queues but cannot read or write documents in any shared namespace.

The namespace grant format mirrors the queue grant format:

```yaml
spec:
  namespaces:
  - pattern: /shared/config/
    matchType: Prefix
    allowedCallers:
    - labels:
        team: platform    # all platform services can read shared config docs
```

Unlike queues, there is no automatic namespace auto-grant — a service has access
only to namespaces explicitly listed in an `EntroQQueue` policy (plus whatever
its own queue prefix covers at the queue level). This keeps document access
opt-in.

---

## Deployment

### Prerequisites

- Go 1.24+
- `kubectl` connected to a Kubernetes cluster
- Docker (for building the operator image)

### Install CRDs

```bash
make install
```

### Build and deploy the operator

```bash
make docker-build docker-push IMG=<registry>/eqk8s:tag
make deploy IMG=<registry>/eqk8s:tag
```

### Apply the sample resources

```bash
kubectl apply -k config/samples/
```

This creates an example `EntroQIdentity` and an `EntroQQueue` (with both queue
and namespace policies) in the `default` namespace.

### Configure OPA

Load the core and k8s provider policies — **not** the OIDC provider; the two
provider sets define overlapping packages and will conflict if loaded together.

```bash
opa run --server \
  --bundle ./conf/core/ \
  --bundle ./conf/providers/k8s/
```

Point the EntroQ server at it:

```bash
eqpg serve \
  --authz opahttp \
  --opa_url http://localhost:8181 \
  --opa_path /v1/data/entroq/authz \
  ...
```

The Helm chart (`charts/entroq/`) wires all of this together automatically.

---

## Uninstalling

```bash
kubectl delete -k config/samples/   # remove sample CRs
make undeploy                        # remove the operator
make uninstall                       # remove the CRDs
```

---

## Contributing

Run the unit tests (no cluster required):

```bash
go test ./...
```

Run the envtest-based controller tests:

```bash
make test
```

After changing types in `api/v1alpha1/`, regenerate deepcopy functions and CRD
manifests:

```bash
make generate   # regenerates zz_generated.deepcopy.go
make manifests  # regenerates config/crd/bases/ and config/webhook/
```

Then copy the updated CRDs to the Helm chart:

```bash
cp config/crd/bases/*.yaml charts/entroq/crds/
```

---

## License

Copyright 2026. Apache License 2.0. See the repository root for the full text.

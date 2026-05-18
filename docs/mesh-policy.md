# EntroQ Mesh Policy

The eqk8s operator maintains an OPA authorization policy for the EntroQ service
mesh. Services prove their identity with a Kubernetes service account JWT; OPA
decides whether they may access a given queue.

Two CRDs control the policy:

- **`EntroQIdentity`** — maps a service account to a set of mesh label claims.
- **`EntroQQueue`** — declares which queues exist and which callers may access them.

The operator watches both across all namespaces, builds a mesh document, and
pushes it to OPA on every change and on a periodic resync.

---

## Queue naming convention

By convention, queues follow the path `/<namespace>/<service>/<leaf>`, for example:

```
/payments/svc-b/inbox         inbound work for svc-b
/payments/svc-b/inbox/response/...  per-request response queues (managed by eqlink)
```

This convention is not enforced by the operator but is required for the
auto-grant (see below) to work correctly.

---

## Auto-grant

Every authenticated service account automatically receives `ALL` access to its
own queue prefix without any CRD configuration. A service account
`system:serviceaccount:payments:svc-a` is auto-granted `ALL` on `/payments/svc-a/`.

This covers:
- The service's own inbox (`/payments/svc-a/inbox`)
- Its response queues (`/payments/svc-a/inbox/response/...`)
- Any other queues it owns under its prefix

No `EntroQQueue` or `EntroQIdentity` resource is needed for a service to manage
its own queues.

---

## EntroQIdentity

Declares the mesh label claims for one or more service accounts within a namespace.

```yaml
apiVersion: entroq.entroq.io/v1alpha1
kind: EntroQIdentity
metadata:
  name: frontend-identities
  namespace: payments
spec:
  identities:
  - serviceAccount: svc-a
    labels:
      group: frontend
      team: payments
  - serviceAccount: svc-c
    labels:
      group: internal-tools
      team: payments
```

### Fields

| Field | Required | Description |
|---|---|---|
| `spec.identities` | yes | List of service account to label mappings (at least one). |
| `identities[].serviceAccount` | yes | Name of the k8s service account in this namespace. |
| `identities[].labels` | yes | Map of label key/value pairs asserted for this account. |

The resulting identity key in the mesh document is
`system:serviceaccount:<namespace>:<serviceAccount>`. This is what the OPA
policy looks up when a service presents its JWT.

---

## EntroQQueue

Declares which queue patterns exist and which callers are permitted to access them.

```yaml
apiVersion: entroq.entroq.io/v1alpha1
kind: EntroQQueue
metadata:
  name: svc-b-policy
  namespace: payments
spec:
  queues:
  - pattern: /payments/svc-b/inbox
    matchType: Exact
    allowedCallers:
    - labels:
        group: frontend
    - labels:
        group: internal-tools
        team: payments
```

### Fields

| Field | Required | Description |
|---|---|---|
| `spec.queues` | yes | List of queue patterns (at least one). |
| `queues[].pattern` | yes | The queue path or prefix to match. |
| `queues[].matchType` | no | `Exact` (default) or `Prefix`. |
| `queues[].allowedCallers` | yes | List of label matchers (at least one). |
| `allowedCallers[].labels` | yes | Map of label key/value pairs that a caller must carry. |

### matchType

- **`Exact`** — the caller's queue path must match the pattern exactly.
  Pattern must not end with `/`.
- **`Prefix`** — the caller's queue path must start with the pattern.
  Pattern must end with `/`.

### allowedCallers: AND within an entry, OR across entries

A caller is permitted if their labels satisfy **at least one** entry in
`allowedCallers` (OR across entries). Within a single entry, **all** key/value
pairs must match (AND semantics).

Example from the YAML above:

| Caller labels | Permitted? | Reason |
|---|---|---|
| `{group: frontend}` | ✓ | satisfies the first entry (group=frontend) |
| `{group: internal-tools, team: payments}` | ✓ | satisfies the second entry (both keys match) |
| `{group: internal-tools}` | ✗ | second entry requires team=payments; first entry not satisfied |
| `{group: backend}` | ✗ | no entry satisfied |

---

## Worked example

**Scenario:** `svc-a` (frontend team) and `svc-c` (internal tools, payments team)
both need to call `svc-b`'s inbox. `svc-b` also has a shared read queue that
any internal service may access.

```yaml
# identities.yaml
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
      team: payments
  - serviceAccount: svc-c
    labels:
      group: internal-tools
      team: payments
```

```yaml
# queues.yaml
apiVersion: entroq.entroq.io/v1alpha1
kind: EntroQQueue
metadata:
  name: svc-b-policy
  namespace: payments
spec:
  queues:
  # svc-b's inbox: frontend and internal-tools/payments may call it
  - pattern: /payments/svc-b/inbox
    matchType: Exact
    allowedCallers:
    - labels:
        group: frontend
    - labels:
        group: internal-tools
        team: payments
  # shared read queue: any payments team member may access
  - pattern: /payments/shared/
    matchType: Prefix
    allowedCallers:
    - labels:
        team: payments
```

---

## Verifying your policy

After applying CRDs, check what the operator pushed to OPA:

```bash
kubectl port-forward -n entroq-system svc/entroq 8182:8181 &
curl -s http://localhost:8182/v1/data/mesh | jq .
```

The response shows `initialized: true` once the operator has reconciled, plus
the full `identities` and `queues` sections. If `initialized` is absent or
false, the operator hasn't reconciled yet — check its logs:

```bash
kubectl logs -n eqk8s-system -l app.kubernetes.io/component=operator
```

To test a specific authorization decision:

```bash
TOKEN=$(kubectl create token svc-a -n payments)
curl -s -X POST http://localhost:8182/v1/data/entroq/authz \
  -H 'Content-Type: application/json' \
  -d "{\"input\":{
    \"authz\":{\"type\":\"Bearer\",\"credentials\":\"$TOKEN\"},
    \"queues\":[{\"exact\":\"/payments/svc-b/inbox\",\"actions\":[\"INSERT\"]}]
  }}" | jq .result
```

`allow: true` means the policy permits it. `failed` lists the queues that were
denied and why.

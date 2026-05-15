# EntroQ Helm Chart

Deploys the EntroQ queue-based service mesh:

- **eqk8s operator** — watches `EntroQQueue` and `EntroQIdentity` CRDs,
  maintains the OPA mesh authorization policy
- **EntroQ server** — gRPC task queue server with an OPA sidecar for
  k8s service-account JWT authorization

## Prerequisites

- Helm 3
- A k8s cluster (see Minikube below for local development)
- Images built and available to the cluster (see below)

## Quick Start (Minikube)

### 1. Start a fresh cluster

```bash
minikube start
```

### 2. Build images into Minikube's Docker daemon

Point your shell's Docker CLI at Minikube so images land where k8s can find them:

```bash
eval $(minikube docker-env)
docker build -t entroq-mem:dev      -f cmd/eqmem/Dockerfile .
docker build -t entroq-operator:dev -f cmd/eqk8s/Dockerfile .
```

Repeat this after any source change. Images are built directly into
Minikube's daemon so no registry or `imagePullPolicy: Never` gymnastics
are required — `IfNotPresent` (the chart default) finds them immediately.

### 3. Sync chart files and install

```bash
make helm-sync   # copies Rego files + CRDs into the chart (incremental)
helm install entroq ./charts/entroq \
  --set oidcDiscovery.grantAnonymous=true \
  --set entroq.images.mem.tag=dev \
  --set operator.image.tag=dev
```

The `oidcDiscovery.grantAnonymous=true` flag grants anonymous read access
to the k8s OIDC JWKS endpoint. Minikube denies this by default; most
managed clusters (GKE, EKS, AKS) already allow it.

The `*.tag=dev` overrides tell the chart to use the locally built `dev`-tagged
images instead of the release tag. Omit them when installing a released chart.

### 4. Verify the stack

```bash
kubectl get pods -A
# entroq-system: entroq-* (2/2 Running)
# eqk8s-system:  eqk8s-controller-manager-* (1/1 Running)
```

### 5. Apply mesh policy CRDs

```bash
kubectl apply -f cmd/eqk8s/config/samples/entroq_v1alpha1_entroqqueue.yaml
kubectl apply -f cmd/eqk8s/config/samples/entroq_v1alpha1_entroqidentity.yaml
```

Verify the operator pushed the mesh document to OPA:

```bash
kubectl port-forward -n entroq-system svc/entroq 8182:8181 &
curl -s http://localhost:8182/v1/data/mesh | jq .
# expect: initialized=true, identities and queues populated
```

### 6. Test authorization

```bash
kubectl create serviceaccount svc-a -n default
TOKEN=$(kubectl create token svc-a -n default)

kubectl port-forward -n entroq-system svc/entroq 37706:37706 &

# Should succeed -- svc-a has group=frontend which satisfies the queue policy
go run ./cmd/eqc --svcaddr localhost:37706 \
  --authz_token "$TOKEN" \
  --claimant "system:serviceaccount:default:svc-a#test" \
  ins -q /payments/svc-b/inbox '{}'

# Should be denied -- stranger has no mesh identity
kubectl create serviceaccount stranger -n default
TOKEN2=$(kubectl create token stranger -n default)
go run ./cmd/eqc --svcaddr localhost:37706 \
  --authz_token "$TOKEN2" \
  --claimant "system:serviceaccount:default:stranger#test" \
  ins -q /payments/svc-b/inbox '{}'
```

## Storage Backends

### memory (default)

No external dependencies. Data is lost on pod restart. Good for development
and stateless workloads.

```bash
helm install entroq ./charts/entroq
```

### journal (durable in-process)

Persists tasks to a WAL journal on a PersistentVolumeClaim. Uses a
StatefulSet. No external database required.

```bash
helm install entroq ./charts/entroq \
  --set entroq.backend.type=journal \
  --set entroq.storage.size=10Gi
```

### postgres

Requires a PostgreSQL instance already running and reachable from the cluster.
This chart does not provision PostgreSQL — deploy it separately using the
[Bitnami chart](https://github.com/bitnami/charts/tree/main/bitnami/postgresql),
[CloudNativePG](https://cloudnative-pg.io/), or your own installation.

**Development** (password stored in Helm release history — not for production):
```bash
helm install entroq ./charts/entroq \
  --set entroq.backend.type=postgres \
  --set entroq.postgres.addr=postgres:5432 \
  --set entroq.postgres.database=entroq \
  --set entroq.postgres.user=entroq \
  --set entroq.postgres.password=mypassword
```

**Production** — create the Secret before installing the chart, then point
the chart at it. The password never enters Helm's release history:
```bash
# 1. Create the secret (do this once, independently of Helm)
kubectl create secret generic entroq-pg-credentials \
  --from-literal=password=mypassword \
  -n entroq-system

# 2. Install the chart, referencing the existing secret
helm install entroq ./charts/entroq \
  --set entroq.backend.type=postgres \
  --set entroq.postgres.addr=postgres:5432 \
  --set entroq.postgres.database=entroq \
  --set entroq.postgres.user=entroq \
  --set entroq.postgres.existingSecret=entroq-pg-credentials
```

The chart skips creating a Secret when `existingSecret` is set, and mounts
the password as the `PGPASSWORD` environment variable instead of passing it
as a CLI argument (which would be visible in `ps` output).

### redis (experimental)

Requires a Redis instance already running and reachable from the cluster.
This chart does not provision Redis. Same secret pattern as postgres. Use `entroq.redis.existingSecret` in production:

```bash
kubectl create secret generic entroq-redis-credentials \
  --from-literal=password=mypassword \
  -n entroq-system

helm install entroq ./charts/entroq \
  --set entroq.backend.type=redis \
  --set entroq.redis.addr=redis:6379 \
  --set entroq.redis.existingSecret=entroq-redis-credentials
```

## Configuration

Key values — override with `--set key=value` or `-f my-values.yaml`:

| Value | Default | Description |
|---|---|---|
| `entroq.storage.enabled` | `false` | `true` = StatefulSet + PVC (journaled); `false` = Deployment (memory-only) |
| `entroq.storage.size` | `1Gi` | PVC size when storage is enabled |
| `entroq.storage.storageClass` | `""` | StorageClass for PVC; blank = cluster default |
| `operator.opaUrl` | `http://entroq.entroq-system.svc.cluster.local:8181` | OPA endpoint the operator pushes mesh documents to |
| `operator.resyncInterval` | `5m` | How often to re-push the mesh document regardless of CRD changes |
| `oidcDiscovery.grantAnonymous` | `false` | Set `true` on Minikube or clusters that restrict JWKS access |
| `entroq.opa.decisionLogs` | unset | Set `true` to emit structured auth decisions to stdout (verbose) |
| `entroq.opa.debug` | unset | Set `true` for OPA debug logging (reveals JWKS fetch failures etc.) |
| `entroq.auth.jwksUrl` | cluster default | Override for non-standard cluster OIDC configurations |

See `values.yaml` for the full set of options.

## Development Workflow

After changing Rego policy or CRD types:

```bash
# Regenerate CRDs from kubebuilder markers
cd cmd/eqk8s && make manifests && cd ../..

# Rebuild operator image
eval $(minikube docker-env)
docker build -t entroq-operator:dev -f cmd/eqk8s/Dockerfile .

# Sync chart and upgrade
make helm-sync
helm upgrade entroq ./charts/entroq --set oidcDiscovery.grantAnonymous=true --set entroq.images.mem.tag=dev --set operator.image.tag=dev

# Bounce the operator pod to pick up the new image
kubectl rollout restart deployment -n eqk8s-system eqk8s-controller-manager
```

After changing Rego files only (no image rebuild needed):

```bash
make helm-sync
helm upgrade entroq ./charts/entroq --set oidcDiscovery.grantAnonymous=true --set entroq.images.mem.tag=dev --set operator.image.tag=dev
kubectl rollout restart deployment -n entroq-system entroq
```

## Uninstall

```bash
helm uninstall entroq
```

Note: Helm does not delete CRDs on uninstall. To remove them manually:

```bash
kubectl delete crd entroqqueues.entroq.entroq.io entroqidentities.entroq.entroq.io
```

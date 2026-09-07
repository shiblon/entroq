# Greetings Demo

A three-service example showing how ordinary HTTP microservices communicate
through an EntroQ mesh without any queue-awareness in their code.

## Services

```
  [you] --HTTP--> svc-a --[queue]--> svc-b --[queue]--> svc-c
                frontend             greeter           compliment
```

- **svc-a** (frontend): accepts `GET /greet?name=X` from outside the mesh,
  calls svc-b to compose a greeting.
- **svc-b** (greeter): calls svc-c for a compliment, returns
  `{"greeting": "Hello, X!", "compliment": "..."}`.
- **svc-c** (compliment): returns a random compliment. No outbound calls.

Each service is a plain Python HTTP server. None of them know about queues.
Each pod has an **eqlink sidecar** that translates outbound HTTP calls into
queue insertions and inbound queue tasks into HTTP calls to the local service.

## Authorization

The `greetings` namespace declares which services may call which:

- svc-a (`tier: frontend`) may call svc-b.
- svc-b (`tier: backend`) may call svc-c.
- svc-a may **not** call svc-c directly -- it has no `tier: backend` label.
- Any service account with no identity entry is denied everywhere.

See `k8s/identities.yaml` and `k8s/queues.yaml`.

## Prerequisites

- Minikube running with EntroQ installed (`helm install entroq ./charts/entroq`)
- `eval $(minikube docker-env)` so images land in Minikube's daemon

## Build images

```bash
eval $(minikube docker-env)
docker build -t greetings-svc-a:dev examples/greetings-demo/svc-a
docker build -t greetings-svc-b:dev examples/greetings-demo/svc-b
docker build -t greetings-svc-c:dev examples/greetings-demo/svc-c
docker build -t entroq-link:dev          -f cmd/eqlink/Dockerfile .
```

## Deploy

```bash
kubectl apply -f examples/greetings-demo/k8s/namespace.yaml
kubectl apply -f examples/greetings-demo/k8s/identities.yaml
kubectl apply -f examples/greetings-demo/k8s/queues.yaml
kubectl apply -f examples/greetings-demo/k8s/svc-c.yaml
kubectl apply -f examples/greetings-demo/k8s/svc-b.yaml
kubectl apply -f examples/greetings-demo/k8s/svc-a.yaml
```

Wait for pods to be ready:

```bash
kubectl get pods -n greetings
```

## Try it

Forward svc-a's port to localhost:

```bash
kubectl port-forward -n greetings deployment/svc-a 8000:8000 &
```

Send a greeting request:

```bash
curl "http://localhost:8000/greet?name=Alice"
```

Expected response:

```json
{
  "greeting": "Hello, Alice!",
  "compliment": "Your queue names are impeccably chosen."
}
```

## Verify authorization

To see a denial in action, create a service account with no mesh identity and
try to call svc-b's inbox directly:

```bash
kubectl create serviceaccount stranger -n greetings
TOKEN=$(kubectl create token stranger -n greetings)
kubectl port-forward -n entroq-system svc/entroq 37706:37706 &
go run ./cmd/eqc --svcaddr localhost:37706 --authz_token "$TOKEN" \
  ins -q /greetings/svc-b/inbox -v '{}'
# Expected: permission denied
```

## How routing works

svc-a calls svc-b with a plain HTTP request to `http://svc-b.localhost:8080/greet`.
The pod's `hostAliases` entry resolves `svc-b.localhost` to `127.0.0.1`, so the
request reaches the local eqlink sender. eqlink strips `.localhost`, sees one
label (`svc-b`), prepends the configured `--namespace=greetings`, and inserts
a task into `/greetings/svc-b/inbox`. svc-b's eqlink receiver claims the task,
calls `http://localhost:8000/greet` on the local Python process, and enqueues
the response for svc-a to pick up.

Neither service knows any of this happened.

## Scale svc-c to zero

The leaf service can be idle at zero replicas until a request reaches
`/greetings/svc-c/inbox`. This optional example requires:

- KEDA installed in the cluster.
- Prometheus Operator and a Prometheus instance configured to select the
  EntroQ `ServiceMonitor`.

Enable metrics discovery when installing or upgrading EntroQ:

```bash
helm upgrade --install entroq ./charts/entroq \
  --set entroq.metrics.serviceMonitor.enabled=true
```

The example scaler uses the common kube-prometheus-stack address
`prometheus-operated.monitoring.svc.cluster.local:9090`. Edit `serverAddress`
in `k8s/svc-c-autoscaling.yaml` if Prometheus has a different address, then
apply it:

```bash
kubectl apply -f examples/greetings-demo/k8s/svc-c-autoscaling.yaml
kubectl get scaledobject,hpa,pods -n greetings --watch
```

After five idle minutes, KEDA scales the `svc-c` Deployment to zero. The next
greeting leaves a durable task in svc-c's EntroQ inbox; Prometheus observes the
queue and KEDA starts one replica. The one-minute scrape and polling intervals
can take nearly two minutes in the worst alignment, so the svc-a and svc-b
eqlink senders use request timeouts long enough to cover observation and pod
startup.

The Prometheus query counts available and claimed tasks, but not future tasks.
That wakes svc-c for ready work, keeps its pod alive while a request is in
flight, and lets delayed tasks remain dormant until they become available.

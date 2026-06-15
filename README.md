# EntroQ

EntroQ is a fault-tolerant, competing-consumer task queue with strongly
transactional semantics: a claimed task cannot be dropped on worker crash, and
results cannot be committed more than once. Workers scale horizontally without
coordination: just add more.

As of v1.0.0, EntroQ also ships a **Kubernetes service mesh** that makes
ordinary HTTP microservices asynchronous without any queue code in the services
themselves. A sidecar intercepts outbound HTTP calls and routes them through
queues transparently.

Go, Python, TypeScript, and Elixir clients. PostgreSQL, Redis, and in-memory
backends. Kubernetes deployment via Helm.

---

Pronounced "Entro-Q" ("Entro-Queue"), as in the letter that comes after
"Entro-P", the next step in managing systems complexity.

It's the right way to outsource reliability and consistency guarantees when working in a competing-consumer environment where exactly-once semantics are needed and work should never be lost. It enables infinite composability, and the microservice mesh built on top of it showcases just one of the many powerful ways it can be used.

Background: [Asynchronous Thinking for Microservice System Design](https://github.com/shiblon/entroq/wiki/Asynchronous-Thinking-for-Microservice-System-Design)
Go docs: [pkg.go.dev/github.com/shiblon/entroq](https://pkg.go.dev/github.com/shiblon/entroq) | [CHANGELOG](CHANGELOG.md)

## Core Concepts

EntroQ simplifies distributed task management by narrowing the entire mutating
interface down to two atomic operations: **Claiming** an available task and
**Modifying** (inserting, deleting, or changing) a set of tasks. Every one of
these operations is wrapped in a version-locked transaction. The moment a task
is claimed or modified, its version increments, which ensures that if one
worker succeeds in a mutation, all other workers holding an older version will
naturally fail. This "Commit Once" semantic eliminates the risk of work getting
dropped after delivery or, more dangerously, being committed to a downstream
system more than once.

Progress in EntroQ is counterintuitive in its simplicity. While many systems
favor strict FIFO ordering, EntroQ selects available tasks **randomly**. This
prevents "poison pill" tasks from rising to the head of the line and starving
the entire cluster; if a task causes a worker to crash, it eventually times out
and is returned to the pool where it is likely to be picked up by another
worker while others continue making progress on "good" work. This design allows
you to scale your processing power simply by adding more workers. Once a task
is in a queue, no further communication between nodes is required.

Workers can set configuration options that automatically quarantine tasks if
they have failed too many times, where they can be inspected manually, fixed,
and reintroduced to their work queue as needed.

## Getting Started

The fastest way to see these concepts in action is using the provided Docker sandbox.

### Sandbox (Docker Compose)
The sandbox provides a complete environment: PostgreSQL, the EntroQ service, Prometheus, and Grafana for monitoring.

```bash
docker compose up --build
```
The service will be available at:
- **gRPC**: `localhost:37706`
- **HTTP/JSON API**: `localhost:9100`
- **Prometheus Metrics**: `localhost:9100/metrics`
- **Grafana Dashboards**: `localhost:3000`

### Command Line
You can poke at the running service using the Go-based command line client:

```bash
go install github.com/shiblon/entroq/cmd/eqc@latest
eqc --help
```

The Python client installs from the `clients/py` subdirectory:
```bash
python3 -m pip install "git+https://github.com/shiblon/entroq#subdirectory=clients/py"
```

### Shell Workers
`eqc work` is a small worker protocol for shell scripts and other local
commands. It is meant for leaf work and simple work-then-respond flows:
one input task is claimed, its JSON value is written to the command's stdin
with a trailing newline, and each non-empty stdout line is parsed as one JSON
output task.

In one terminal, start a worker:

```bash
eqc work -q /demo/in -Q /demo/out -c 'printf "{\"seen\":%s}\n" "$(cat)"'
```

In another terminal, insert work and read the response:

```bash
eqc ins -q /demo/in -v '{"name":"Ada"}'
eqc ts -q /demo/out
```

Use `-- COMMAND [ARG...]` instead of `-c` when you do not need a shell. This
worker copies each input task value to `/demo/out`:

```bash
eqc work -q /demo/in -Q /demo/out -- cat
```

There is no implicit copy mode; `cat` is the explicit command in this example.

Important details:

- `-q, --queue` can be repeated; the worker claims one task at a time from any
  listed input queue.
- `-Q, --out-queue` is the single queue for all stdout JSONL output tasks.
- `--in` delays output tasks, matching `eqc ins --in`.
- On success, the input task is deleted and all stdout tasks are inserted in
  the same `Modify`.
- On command failure, the current task is modified in place: `attempt` and
  `err` are updated, and `At` is moved by `--retry-in`. The default
  `--max-attempts 0` means unlimited retries.
- Invalid JSONL output, too much stdout, or exhausted attempts move the input
  task to `--error-queue`, defaulting to `<input>/err`.
- `--recur-in` is the cron-like mode: after a successful run, the claimed task
  is deleted and a fresh copy of its input value is inserted back into the same
  input queue with that relative delay.

Scripts should write logs to stderr. They may call `eqc` for ordinary client
operations, including document claims, but those operations are not part of the
worker's final atomic `Modify`; claimed docs also need to be used within their
original lease because `eqc work` does not renew them for the script.

## Language Clients

EntroQ defines a simple protocol: `claim -> work -> modify`. All clients follow this transactional loop.

### Go
The Go client is the reference implementation and supports automatic background task renewal.

```go
svc, _ := entroq.New(ctx, eqgrpc.Opener("localhost:37706", eqgrpc.WithInsecure()))
defer svc.Close()

w := worker.New(svc,
    worker.WithDoModify(func(ctx context.Context, task *entroq.Task, val json.RawMessage, docs []*entroq.Doc) ([]entroq.ModifyArg, error) {
        log.Printf("Processing: %s", string(task.Value))
        return []entroq.ModifyArg{task.Delete()}, nil // finish by deleting
    }),
)
w.Run(ctx, worker.Watching("/my/queue"))
```

### Python
The Python client is async (`asyncio`). A worker claims tasks, renews the claim
in the background while your handler runs, and atomically applies the
`Modification` your handler returns.

```python
import asyncio
from entroq.json import EntroQJSON
from entroq.types import Modification
from entroq.worker import EntroQWorker

async def main():
    client = EntroQJSON("http://localhost:9100")

    @EntroQWorker.handler
    async def process(task, docs):
        print(f"Processing: {task.value}")
        return Modification(Modification.deleting(task))  # finish by deleting

    worker = EntroQWorker(client, "/my/queue")
    await worker.run(process)

asyncio.run(main())
```

### TypeScript (Node.js)
The TypeScript client provides a worker abstraction for modern async/await environments. See [`clients/js/README.md`](clients/js/README.md) for installation and full API docs.

```typescript
import { EntroQClient, EntroQWorker } from "entroq";

const client = new EntroQClient({ baseUrl: "http://localhost:9100" });
const worker = new EntroQWorker(client);

await worker.run(["/my/queue"], async (task) => {
    console.log("Processing:", task.value);
    // Return a modification to apply atomically; here we delete to finish.
    return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
});
```

### Elixir
The Elixir client provides an HTTP/JSON client and a worker abstraction that
renews task/doc claims while `perform/2` runs, then applies returned
modifications after renewal stops and final versions are stable. See
[`clients/elixir/README.md`](clients/elixir/README.md) for details.

```elixir
defmodule MyWorker do
  use EntroQ.Worker

  @impl true
  def perform(task, _docs) do
    {:modify, EntroQ.Modification.delete(task)}
  end
end

client = EntroQ.new("http://localhost:9100")
EntroQ.Worker.run(client, ["/my/queue"], MyWorker)
```

## Document Store

In addition to tasks, EntroQ provides a key-value document store that shares
the same atomic transaction space. A doc has a namespace, an ID, a primary key,
an optional secondary key, a JSON content field, and an optional expiration time.

Unlike tasks, docs are not work items. They are durable shared state:
configuration, counters, reduce output, or any data that multiple workers need
to read or coordinate around.

Docs and tasks share the same `Modify` call, so you can atomically insert a
task and its initial doc state, or delete a task and update a doc, in one round
trip with no possibility of partial failure.

`ClaimDocs` acquires an exclusive lease on all docs sharing a primary key in
one atomic operation, making the primary key the natural unit of coordinated
ownership. Results are returned ordered by `(primary key, secondary key)`,
which makes reduce-shaped strategies straightforward.

## Reusable Workers

`pkg/workers` provides ready-made worker implementations for common patterns,
so you don't have to write the claim/renew/modify loop yourself:

| Package | What it does |
|---|---|
| `batchworker` | Aggregates tasks and processes them in configurable batches |
| `mapworker` | Generic map: transforms a task's value and routes the result to an outbox queue |
| `httpworker` | Executes HTTP requests described by task values; writes the response back as a task |
| `procworker` | Runs a subprocess described by the task value; captures stdout/stderr as an output task |
| `fileworker` | Writes task values to files |
| `appendworker` | Appends task values to a stuffedio WAL journal |

## Kubernetes Service Mesh

EntroQ v1.0.0 ships a Kubernetes operator that turns the queue into a
**transparent async service mesh**. Ordinary HTTP microservices communicate
through queues with no queue-awareness in their code. Each pod gets an
**eqlink sidecar** that intercepts outbound HTTP calls, converts them to queue
insertions, and converts inbound queue tasks back to HTTP calls on the local
service.

```
[you] ──HTTP──▶ svc-a ──[queue]──▶ svc-b ──[queue]──▶ svc-c
               frontend             greeter            compliment
```

A service calls its neighbor with a plain HTTP request to
`http://svc-b.localhost:8080/path`. The pod's `hostAliases` resolve the address
to `127.0.0.1`, so the request hits the local eqlink sender. eqlink strips the
domain suffix, maps the hostname to a queue name, and inserts a task. On the
other side, the receiver's eqlink claims the task, calls the local service over
loopback, and routes the response back through a per-request reply queue.
Neither service knows any of this happened.

Authorization is declared with two CRDs:

- **`EntroQQueue`**: declares which queues a service exposes and which callers
  may enqueue to them, using label-set predicates with AND/OR semantics.
- **`EntroQIdentity`**: maps a Kubernetes service account to a set of mesh
  label claims.

The operator watches these CRDs cluster-wide and maintains an OPA authorization
document that the EntroQ service enforces on every queue operation.

See **[`examples/greetings-demo`](examples/greetings-demo)** for a working
end-to-end example with three Python services, deployment manifests, and a
step-by-step walkthrough.

CRD reference (field-by-field, worked examples, policy verification):
[`docs/mesh-policy.md`](docs/mesh-policy.md)

## Production Deployment (Helm)

For Kubernetes environments, a Helm v3 chart is available in `charts/entroq`. It supports backend toggling and secure secret management. See [`charts/entroq/README.md`](charts/entroq/README.md) for full options. Postgres is shown below. In-memory (journaled) and Redis-backed are also available.

```bash
make helm-sync
helm install my-queue ./charts/entroq --set entroq.backend.type=postgres
```

---

## Technical Protocol Details

### Tasks
A task is defined by a Queue Name, a Globally Unique ID, a Version, an Arrival Time, and a Value. The version increments every time the task is mutated, providing the foundation for transactional safety.

### Queues
Queues are not first-class entities; they spring into existence only when tasks are assigned to them and vanish when empty, allowing for highly dynamic, ad-hoc workflows.

### Claims and Safe Work

When a worker issues a `Claim`, it atomically increments the task's version and
pushes its arrival time into the future, "locking" it for a specific duration.
For long-running work, the best practice is to keep these initial claim times
low and rely on background renewal to maintain the lock.

Any finalization or downstream update is handled by the `Modify` call, which
can include any combination of insertions, deletions, or value changes. If any
single part of the modification fails (perhaps because a task's version has
drifted or a dependency was not met), the entire operation rolls back. This
requires a shift in how you think about "Safe Work": you should aim for
idempotence, perhaps by writing results to unique, timestamped files before
committing the final task deletion. The rule is simple: only mutate tasks you
have successfully claimed, and always assume your work might be retried.

## Backends

EntroQ is backend-agnostic. The Go library supports:

- **In-memory**: Perfect for testing or light-duty singleton services (includes a WAL journal).
- **PostgreSQL**: Production-grade persistence using `SKIP LOCKED` for high performance.
- **gRPC**: A client that talks to a remote `eqpg`, `eqmem`, or `eqredis` service instance.

## Authorization

EntroQ integrates with [Open Policy Agent (OPA)](https://www.openpolicyagent.org/)
for queue-level access control. Enable it with `--authz=opahttp` on any service
binary. On every operation the server packages the request (queues + actions +
`Authorization` header) into a JSON document, POSTs it to OPA, and allows or
denies based on the response.

Two provider sets ship with the repo:

| Deployment | Provider | Identity source |
|---|---|---|
| Standalone / OIDC | `pkg/authz/opadata/conf/providers/entroq/` | JWT from any standard OIDC IDP |
| Kubernetes mesh | `pkg/authz/opadata/conf/providers/k8s/` | Pod service account tokens + `EntroQQueue`/`EntroQIdentity` CRDs |

For the Kubernetes mesh path, the eqk8s operator maintains the OPA authorization
document automatically from CRDs, with no manual data wiring needed when using
the Helm chart.

Full configuration guide, IDP-specific settings, and examples:
[`pkg/authz/opadata/OPA_AUTHZ.md`](pkg/authz/opadata/OPA_AUTHZ.md)

Runnable sandbox: [`examples/authz/`](examples/authz/)

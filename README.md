# EntroQ

A task queue with strong competing-consumer semantics and transactional updates.

See here for an article explaining how this might fit into your system:

[Asynchronous Thinking for Microservice System Design](https://github.com/shiblon/entroq/wiki/Asynchronous-Thinking-for-Microservice-System-Design)

The Go components of this package can be found in online documentation for the [github.com/shiblon/entroq](https://pkg.go.dev/github.com/shiblon/entroq) Go package.

Pronounced "Entro-Q" ("Entro-Queue"), as in the letter that comes after "Entro-P". We aim to take the next step away from parallel systems chaos. It is also the descendant of `github.com/shiblon/taskstore`, an earlier and less robust attempt at the same idea.

It is designed to be as simple and modular as possible, doing one thing well. It is not a pubsub system, a database, or a generic RPC mechanism. It is only a competing-consumer unordered work queue manager, and will only ever be that.

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
you to scale your processing power simply by adding more workers--once a task
is in a queue, no further communication between nodes is required.

Workers can set configuration options that automatically quarantine tasks if
they have failed too many times, where they can be inspected manually, fixed,
and reintroduced to their work queue as needed.

## Evaluation & Getting Started

The fastest way to see these concepts in action is using the provided Docker sandbox.

### Sandbox (Docker Compose)
The sandbox provides a complete environment: PostgreSQL (with the `entroq` schema), the EntroQ service (ConnectRPC), and Prometheus for monitoring.

```bash
docker compose up --build
```
The service will be available at:
- **gRPC**: `localhost:37706`
- **HTTP/JSON API**: `localhost:9100`
- **Prometheus Metrics**: `localhost:9100/metrics`

### Command Line
You can poke at the running service using the Go-based command line client:

```bash
go install github.com/shiblon/entroq/cmd/eqc@latest
eqc --help
```

There is also a Python-based CLI:
```bash
python3 -m pip install git+https://github.com/shiblon/entroq
python3 -m entroq --help
```

## Language Clients

EntroQ defines a simple protocol: `claim -> work -> modify`. All clients follow this transactional loop.

### Go
The Go client is the reference implementation and supports automatic background task renewal.

```go
svc, _ := entroq.New(ctx, eqgrpc.Opener("localhost:37706"))
defer svc.Close()

w := worker.New(svc,
    worker.WithDo(func(ctx context.Context, task *entroq.Task) error {
        log.Printf("Processing: %s", string(task.Value))
        return nil // success
    }),
    worker.WithFinish(func(ctx context.Context, task *entroq.Task) error {
        _, _, err := svc.Modify(ctx, task.Delete())
        return err // finish
    }),
)
w.Run(ctx, "/my/queue")
```

### Python
The Python client provides a flexible worker abstraction using context managers to handle task heartbeating and finalization.

```python
from entroq.json import EntroQJSON
from entroq.worker import EntroQWorker

client = EntroQJSON("http://localhost:9100")
worker = EntroQWorker(client)

def handle(renew, finalize):
    with renew as task:
        # Task is automatically renewed in the background here
        print(f"Processing: {task.value}")

    with finalize as task:
        # Renewal has stopped; task version is now stable
        client.modify(deletes=[task.as_id()])

worker.work("/my/queue", handle)
```

### TypeScript (Node.js)
The TypeScript client provides a worker abstraction for modern async/await environments.

```typescript
import { EntroQClient, EntroQWorker } from "@shiblon/entroq";

const client = new EntroQClient({ baseUrl: "http://localhost:9100" });
const worker = new EntroQWorker(client);

await worker.run(["/my/queue"], async (task) => {
    console.log("Processing:", Buffer.from(task.value, "base64").toString());
    return "delete"; // automatically deletes upon completion
});
```

## Production Deployment (Helm)

For Kubernetes environments, a Helm v3 chart is available in `deploy/helm/entroq`. It supports backend toggling and secure secret management.

```bash
helm install my-queue ./deploy/helm/entroq --set backend=pg
```

---

## Technical Protocol Details

### Tasks
A task is defined by a Queue Name, a Globally Unique ID, a Version, an Arrival Time, and a Value. The version increments every time the task is mutated, providing the foundation for our transactional safety.

### Queues
Queues are not first-class entities; they exist only as long as tasks are assigned to them. If a queue has no tasks, it effectively does not exist. This allows for dynamic, ad-hoc queue creation.

### Claims
## Technical Protocol and Safe Work

At its heart, an EntroQ task is a state machine defined by its Queue, a
globally unique ID, a version, and an arrival time. Queues themselves are not
first-class entities; they spring into existence only when tasks are assigned
to them and vanish when empty, allowing for highly dynamic, ad-hoc workflows.
When a worker issues a `Claim`, it atomically increments the task's version and
pushes its arrival time into the future, "locking" it for a specific duration.
For long-running work, the best practice is to keep these initial claim times
low and rely on background renewal to maintain the lock.

Any finalization or downstream update is handled by the `Modify` call, which
can include any combination of insertions, deletions, or value changes. If any
single part of the modification fails--perhaps because a task's version has
drifted or a dependency was not met--the entire operation rolls back. This
requires a shift in how you think about "Safe Work": you should aim for
idempotence, perhaps by writing results to unique, timestamped files before
committing the final task deletion. The rule is simple: only mutate tasks you
have successfully claimed, and always assume your work might be retried.

## Backends

EntroQ is backend-agnostic. The Go library supports:

- **In-memory**: Perfect for testing or light-duty singleton services (includes a WAL journal).
- **PostgreSQL**: Production-grade persistence using `SKIP LOCKED` for high performance.
- **gRPC**: A client that talks to a remote `qsvc` instance.

## Authorization

The gRPC service supports integration with **Open Policy Agent (OPA)** for fine-grained queue access control. By using the `--authz=opahttp` flag, you can enforce policies based on the `Authorization` header passed by clients.

See `pkg/authz` for more details on structured error handling and OPA policy integration.

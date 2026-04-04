# EntroQ

A task queue with strong competing-consumer semantics and transactional updates.

See here for an article explaining how this might fit into your system:

[Asynchronous Thinking for Microservice System Design](https://github.com/shiblon/entroq/wiki/Asynchronous-Thinking-for-Microservice-System-Design)

The Go components of this package can be found in online documentation for the [github.com/shiblon/entroq](https://pkg.go.dev/github.com/shiblon/entroq) Go package.

Pronounced "Entro-Q" ("Entro-Queue"), as in the letter that comes after "Entro-P". We aim to take the next step away from parallel systems chaos. It is also the descendant of `github.com/shiblon/taskstore`, an earlier and less robust attempt at the same idea.

It is designed to be as simple and modular as possible, doing one thing well. It is not a pubsub system, a database, or a generic RPC mechanism. It is only a competing-consumer unordered work queue manager, and will only ever be that.

## Core Concepts

EntroQ supports precisely two atomic mutating operations:

- Claim an available task from one of possibly several queues, or
- Update a set of tasks (delete, change, insert), optionally depending on the passive existence of other task versions.

Both `Claim` and `Modify` change the version number of every task they affect. Any time any task is mutated, its version increments. Thus, if one process manages to mutate a task, any other process that was working on it will find that it is not available, and will fail. This is the key concept behind the "commit once" semantics of EntroQ.

Unlike many pub/sub systems used as competing consumer queues, this eliminates the possibility of work getting dropped after delivery, or work being committed more than once:

> Work commits should never be lost or duplicated.

### Non-Starvation via Random Selection

Perhaps counterintuitively given the name, *queues are not strictly ordered*. While there is a loose ordering (ready vs. not ready), EntroQ selects a **random** available task when claiming. This prevents a small set of "bad" tasks (e.g., those that cause a worker to crash) from rising to the top and starving the entire system. In EntroQ:

> Workers should continue making progress even when some tasks are bad.

### Scaling without Communication

Once a task is in a queue, that is the only communication mechanism needed. To process things faster, just add more workers. If a worker dies, its task arrival time will eventually pass, and another worker will pick it up automatically.

> You should be able to scale workers without communication.

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

worker := svc.NewWorker(entroq.FuncHandler(
    func(ctx context.Context, task *entroq.Task) error {
        log.Printf("Processing: %s", string(task.Value))
        return nil // success
    },
    func(ctx context.Context, task *entroq.Task) error {
        _, _, err := svc.Modify(ctx, task.AsDeletion())
        return err // finalize
    },
))
worker.Run(ctx, "/my/queue")
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
A `Claim` atomically increments a task's version and advances its arrival time into the future. This "locks" the task for a specific duration. Best practice is to keep initial claim times low and rely on background renewal for long-running work.

### Modify
The `Modify` call is a single atomic transaction that can include:
- **Insert**: Adding new tasks.
- **Delete**: Removing completed tasks.
- **Change**: Moving tasks between queues or updating values.
- **Depend**: Requiring that certain task versions exist without changing them.

If any part of the modification fails (e.g., a version mismatch), the entire operation rolls back.

### Workers & "Commit Once"
Because EntroQ uses a `claim -> modify` lifecycle, a task can only be acknowledged **once**. If two workers attempt to finish the same task, only the one holding the latest version will succeed. The other will receive a dependency error and should discard its work. Idempotent work is your friend, here. For example, if you are writing to files, write to a unique filename based on a timestamp, then pass that to another task to copy it to its final destination.

### Safe Work Principles
- **Idempotence**: Outside mutations should be idempotent (e.g., setting a DB value instead of incrementing it).
- **Unique Outputs**: Any files written should be uniquely named to avoid corruption during retries.
- **Claim First**: Only mutate tasks that you have successfully claimed.

## Backends

EntroQ is backend-agnostic. The Go library supports:

- **In-memory**: Perfect for testing or light-duty singleton services (includes a WAL journal).
- **PostgreSQL**: Production-grade persistence using `SKIP LOCKED` for high performance.
- **gRPC**: A client that talks to a remote `qsvc` instance.

## Authorization

The gRPC service supports integration with **Open Policy Agent (OPA)** for fine-grained queue access control. By using the `--authz=opahttp` flag, you can enforce policies based on the `Authorization` header passed by clients.

See `pkg/authz` for more details on structured error handling and OPA policy integration.

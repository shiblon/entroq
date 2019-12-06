# EntroQ

A task queue with strong competing-consumer semantics and transactional updates.

Pronounced "Entro-Q", as in the letter than comes after "Entro-P". We aim to
take the next step away from parallel systems chaos. It is also the descendent
of `github.com/shiblon/taskstore`, an earlier and less robust attempt at the
same idea.

It is designed to be as simple and modular as possible, doing one thing well.
It is not a pubsub system, a database, or a generic RPC mechanism. It is only a
competing-consumer work queue, and will only ever be that. As such, it has also
been designed to do that one thing really well.

## Use

A Docker container is available on Docker Hub as `shiblon/entroq`. You can use
this to start an EntroQ service, then talk to it using the provided Go or
Python libraries. It exposes port `37706` by default.

If you merely want a single-process in-memory work queue for Go, you can just
use the in-memory implementation of the library without any server at all. For
other languages, you should use the service and the gRPC-based
language-specific library to talk to it.

There is also a command-line client that you can use to talk to the EntroQ
service:

    go install entrogo.com/entroq/cmd/eqc

You can then run `eqc` to talk to an EntroQ service (such as one started in the
`shiblon/entroq` container).

## Concepts

EntroQ supports precisely two atomic mutating operations:

- Claim a random task whose time has arrived, or
- Update a set of tasks (delete, change, insert), optionally depending on other task versions.

There are a few read-only accessors, as well, such as a way to list tasks
within a queue, a way to list queues with their sizes, etc. These operations do
not have any transactional properties, and are best-effort snapshots into queue
state.

All mutating operations (including claims, which change the arrival time of a
task and modify its claimant) modify the task in such a way that, from the
perspective of another process that might also be trying to update it, that
task _no longer exists_. This guarantees "exactly one commit" semantics, although
it might not guarantee that the _work_ represented by a task is done exactly
once. Network partitions can cause work to happen more than once, but the
acknowledgement of that completion will only ever happen once.

A task, at its most basic, is defined by these traits:

- Queue Name
- Globally Unique ID
- Version
- Arrival Time
- Value

Tasks also hold other information, such as the ID of the current client that
holds a claim on it, when it was created, and when it was modified, but these
are implementation details.

When claiming a task, the version is incremented and the arrival time is set to
a time in the future, making it unavailable to other would-be claimants.

EntroQ can hold multiple "queues", though these do not exist as first-class
concepts in the system. A queue only exists so long as there are tasks that
want to be in it. If no tasks are in a particular queue, that queue does not
exist.

Every modification to a task increments its version number, making it possible to
keep track of a task through many modifications by remembering its unique ID.
Any modification or dependency on a task, however, uses _both_ the ID and the
version, so that if one client holds an expired task, and another claims it,
the first cannot then delete or otherwise change it because the version number
no longer matches; its version of the task no longer exists to change.

This allows for a robust competing-consumer worker setup, where workers are
guaranteed to not accidentally clobber each other's tasks.

### Claim

A `Claim` occurs on a queue. A randomly chosen task among those that have an
Arrival Time (AT) in the past is returned if any are available. Otherwise this
call blocks. There is a similar call `TryClaim` that is non-blocking (and
returns a nil value if no task could be claimed immediately), if that is
desired.

### Modify

The `Modify` call can do multiple modifications at once, in an atomic
transaction. There are four kinds of modifications that can be made:

- Insert: add one or more tasks to desired queues.
- Delete: delete one or more tasks.
- Change: change the value, queue, or arrival time of a given task.
- Depend: require presence of one or more tasks and versions.

If any of the dependencies are not present (tasks to be deleted, changed, or
depended upon, or tasks to be inserted with a colliding pre-specified ID), then
the entire operation fails with an error indicating exactly which tasks were no
present in the requested version or had colliding identifiers in the case of
insertion.

### Workers

Once you create an EntroQ client instance, you can use it to create what is
called a "worker". A worker is essentially a thread (or your language's
equivalent) that waits for tasks to become available in a particular queue,
then runs a function of your choice that does the work and returns the desired
end state (e.g., deletion of the completed task).

This is a very common way of using EntroQ: stand up a service, then start up
several workers responsible for handling the flow of tasks through your system.

### Commit Once, Maybe Work Twice

Work in this setup can only be *acknowledged* once. It is possible for more
than one claimant (or "worker") to be performing the same tasks at the same
time, but only one of them will succeed in committing that work to EntroQ.

Consider two workers, which we will call "Early and Slow" (ES) and "Late and
Quick" (LQ). The "Early and Slow" worker is the first to claim a particular
task, but then takes a long time getting it done. This delay may be due to a
network partition, or a slow disk on a machine, memory pressure, or process
restarts. Whatever the reason, ES claims a task, then doesn't acknowledge it
before the deadline.

While ES is busy working on its task, but not acknowleding it, "Late and Quick"
(LQ) successfully claims the task after its arrival time is reached. If, while
it is working on it, ES tries to commit its task, it has an earlier version and
the commit fails. LQ then finishes and succeeds, because it holds the most
recent version of the task, which is the one represented in the queue.

This also works if LQ finishes before ES does, in which case ES tries to
finally commit its work, but the task that is in the queue is no longer there:
it has been deleted because LQ finished it and requested deletion.

These semantics, where a claim is a mutating event, and every alteration to a
task changes its version, make it safe for multiple workers to attempt to do
the same work without the danger of it being committed (reported) twice.

### Safe Work

To further ensure safety when using a competing-consumer work queue like
EntroQ, it is important to adhere to a few simple principles:

- All state mutations should be idempotent, and
- Any output files should be uniquely named.

The first principle of idempotence allows things like database writes to be
done safely by multiple workers (remembering the ES vs. LQ concept above). As
an example, instead of *incrementing* a value in a database, it should simply
be *set to its final value*. Sometimes an increment is really what you want, in
which case you can make that operation idempotent by storing the final value
*in the task itself* so that the worker simply records that. That way, no
matter how many workers make the change, they make it to the same value.

The second principle of unique file names applies to every worker that attempts
to write anything. Each worker, even those working on the same task, should
generate a random or time-based file name for everything that it writes to the
file system. While this can generate garbage that needs to be collected, it
also guarantees that partial writes do not corrupt complete ones. File system
semantics are quite different from database semantics, and having
uniquely-named outputs for every write helps to guarantee that corruption is
avoided.

## Backends

To create a client that can access the task manager, use the `entroq.New`
function and hand it a `BackendOpener`. An "opener" is a function that can,
given a context and suitable parameters, create a backend for the client to use
as its implementation.

There are three primary backend implementations: in-memory, PostgreSQL, and a
gRPC client.

### In-Memory Backend

The `mem` backend allows your `EntroQ` instance to work completely in-process.
You can use exactly the same library calls to talk to it as you would if it
were somewhere on the network, making it easy to start in memory and progress
to persistent networked storage later as needed.

The following is a short example of how to create an in-memory work queue:

```go
package main

import (
  "context"

  "entrogo.com/entroq"
  "entrogo.com/entroq/mem"
)

func main() {
  ctx := context.Background()
  eq := entroq.New(ctx, mem.Opener())

  // Use eq.Modify, eq.Insert, eq.Claim, etc., probably in goroutines.
}
```

### gRPC Backend

The `grpc` backend is somewhat special. It converts an `entroq.EntroQ` client
into a gRPC *client*, where the server is built from `qsvc`, described below.

This allows you to stand up a gRPC endpoint in front of your "real" persistent
backend, giving you authentication management and all of the other goodies that
gRPC provides on the server side, all exposed via protocol buffers and the
standard gRPC service interface.

All clients would then use the grpc backend to connect to this service, again
with gRPC authentication and all else that it provides. This is the preferred
way to use the EntroQ client library.

As a basic example of how to set up a gRPC-based EntroQ client:

```go
package main

import (
  "context"

  "entrogo.com/entroq"
  "entrogo.com/entroq/grpc"
)

func main() {
  ctx := context.Background()
  eq := entroq.New(ctx, grpc.Opener(":37706"))

  // Use eq.Modify, eq.Insert, eq.Claim, etc., probably in goroutines.
}
```

The opener accepts a host name and a number of other gRPC-related optional
parameters, including mTLS parameters and other familiar gRPC controls.

### PostgreSQL Backend

The `pg` backend uses a PostgreSQL database. This is a performant, persistent
backend option.

```go
package main

import (
  "context"

  "entrogo.com/entroq"
  "entrogo.com/entroq/pg"
)

func main() {
  ctx := context.Background()
  eq := entroq.New(ctx, pg.Opener(":5432", pg.WithDB("postgres"), pg.WithUsername("myuser")))
  // The above supports other postgres-related parameters, as well.

  // Use eq.Modify, eq.Insert, eq.Claim, etc., probably in goroutines.
}
```

This backend is highly PostgreSQL-specific, as it requires the ability to issue
a `SELECT ... FOR UPDATE SKIP LOCKED` query in order to performantly claim
tasks. MySQL has similar support, so a similar backend could be written for it
if desired.

### Starting a PostgreSQL instance using Docker Compose

If you wish to start an EntroQ service backed by PostgreSQL, the easiest
approach is to use containers. A `docker-compose` example is given below that
also works wth `docker stack deploy`. Getting the same setup in Kubernetes is
also simple.

Note that we use `/tmp` for the example below. This is not recommended in
production for obvious reasons, but should illuminate the way things fit
together.

```yaml
version: "3"
services:
  database:
    image: "postgres:12"
    deploy:
      restart_policy:
        condition: any
    restart: always
    volumes:
      - /tmp/postgres/data:/var/lib/postgresql/data

  queue:
    image: "shiblon/entroq:v0.2"
    depends_on:
      - database
    deploy:
      restart_policy:
        condition: any
    restart: always
    ports:
      - 37706:37706
    command:
      - "pg"
      - "--dbaddr=database:5432"
      - "--attempts=10"
```

This starts up PostgreSQL and EntroQ, where EntroQ will make multiple attempts
to connect to the database before giving up, allowing PostgreSQL some time to
get its act together.

The `eqc` command-line utility is included in the `entroq` container, so you
can play around with it using `docker exec`. If the container's name is stored
in `$container`:

```
docker exec $container eqc --help
```

Alternatively, you can write code that uses the `gRPC` backend and talks to
`37706` on the localhost.

## QSvc

The qsvc directory contains the gRPC service implementation that is found in
the Docker container `shiblon/entroq`. This service exposes the endpoints found
in `proto`. Working services using various backends are found in the
cmd directories, e.g.,

- `cmd/eqmemsvc`
- `cmd/eqpgsvc`

You can build any of these and start them on desired ports and with desired
backend connections based on flag settings.

There is no *service* backend for `grpc`, though one could conceivably make
sense as a sort of proxy. But in that case you should really just use a grpc
proxy.

When using one of these services, this is your main server and should be
treated as a singleton. Clients should use the `grpc` backend to connect to it.

Does making the server a singleton affect performance? Yes, of course, you can't
scale a singleton, but in practice if you are hitting a work queue that hard you
likely have a granularity problem in your design.

## Python Implementation

In `contrib/py` there is an implementation of a gRPC client in Python, inluding
basic CLI. You can use this to talk to a gRPC EntroQ service, and it includes a
basic worker implementation so that you can relatively easily replace uses of
Celery in Python.

## Examples

One of the most complete examples that is also used as a stress test is a naive
implementation of MapReduce, in `contrib/mr`. If you look in there, you will
see numerous idiomatic uses of the competing queue concept, complete with
workers, using queue empty tests a part of system semantics, queues assigned to
specific processes, and others.

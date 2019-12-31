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
    eqc --help

You can then run `eqc` to talk to an EntroQ service (such as one started in the
`shiblon/entroq` container).

There is also a Python-based command line, installable via pip:

    python3 -m pip install git+https://github.com/shiblon/entroq
    python3 -m entroq --help

## Concepts

EntroQ supports precisely two atomic mutating operations:

- Claim an available task from one or more queues, or
- Update a set of tasks (delete, change, insert), optionally depending on other task versions.

There are a few read-only accessors, as well, such as a way to list tasks
within a queue, a way to list queues with their sizes, etc. These read-only
operations do not have any transactional properties, and are best-effort
snapshots of queue state.

Both `Claim` and `Modify` change the version number of every task they affect.
Any time any task is mutated, its version increments. Thus, if one process
manages to mutate a task, any other process that was working on it will find
that it is not available, and will fail. This is the key concept behind the
"commit once" semantics of EntroQ.

Unlike many pub/sub systems that are used as competing consumer queues, this
eliminates the possibility of work getting dropped after delivery, or work
being committed more than once. Some additional detail on this approach is
given below, but the fundamental principle is this:

    Work commits should never be lost or duplicated.

### Tasks

A task, at its most basic, is defined by these traits:

- Queue Name
- Globally Unique ID
- Version
- Arrival Time
- Value

Tasks also hold other information, such as the ID of the current client that
holds a claim on it, when it was created, and when it was modified, and how
many times it has been claimed, but these are implementation details.

### Queues

EntroQ can hold multiple "queues", though these do not exist as first-class
concepts in the system. A queue only exists so long as there are tasks that
want to be in it. If no tasks are in a particular queue, that queue does not
exist. In database terms, you can think of the queue as being a property of the
task, not the other way around. Indeed, in the Postgres backend, it is simply a
column in a `tasks` table.

Because task IDs are durable, and only the version increments during mutation,
it is possible to track a single task through multiple modifications. Any
attempted mutation, however, will fail if both the ID *and* version don't match
the task to be mutated. The ID of the task causing a modification failure is
returned with the error.

This allows for a robust competing-consumer worker setup, where workers are
guaranteed to not accidentally clobber each other's tasks.

### Claims

A `Claim` occurs on one or more queues. A randomly chosen task among those that
have an Arrival Time (spelled `at`) in the past is returned if any are
available. Otherwise this call blocks. There is a similar call `TryClaim` that
is non-blocking (and returns a nil value if no task could be claimed
immediately), if that is desired.

When more than one queue is specified in a `Claim` operation, only one task
will be returned from one of those queues. This allows a worker to be a fan-in
consumer, fairly pulling tasks from multiple queues.

When successfully claiming a task, the following happen atomically:

- Its version is incremented, and
- Its arrival time is advanced to a point in the future.

Incrementing the version ensures that any previous claimants will fail to
update that task: the version number will not match. Setting the arrival time
in the future gives the new claimant time to do the work, or to establish a
cycle of "at updates", where it renews the lease on the task until it is
finished. It also allows the network time to return the task to the claimant.

Note: best practice is to keep the initial claim time relatively low, then rely
on the claimant to update the arrival time periodically. This allows for
situations like network partitions to be recovered from relatively quickly:
a new worker will more quickly pick up a task that missed a 30-second renewal
window than one that was reserved for 15 minutes with a claimant that died
early in its lease.

### Modify

The `Modify` call can do multiple modifications at once, in an atomic
transaction. There are four kinds of modifications that can be made:

- Insert: add one or more tasks to desired queues.
- Delete: delete one or more tasks.
- Change: change the value, queue, or arrival time of a given task.
- Depend: require presence of one or more tasks and versions.

If any of the dependencies are not present (tasks to be deleted, changed, or
depended upon, or tasks to be inserted when a colliding ID is specified), then
the entire operation fails with an error indicating exactly which tasks were
not present in the requested version or had colliding identifiers in the case
of insertion.

### Workers

Once you create an EntroQ client instance, you can use it to create what is
called a "worker". A worker is essentially a claim-renew-modify loop on one or
more queues. These can be run in goroutines (or threads, or your language's
equivalent). Creating a worker using the standard library allows you to focus
on writing only the logic that happens once a task has been acquired. In the
background the claimed task is renewed for as long as the worker code is
running.

The code that a worker runs is responsible for doing something with the claimed
task, then returning the intended modifications that should happen when it is
successful. For modification of the claimed task, the standard worker code
handles updating the version number in case that task has been renewed in the
background (and thus had its version increment while the work was being done).

This is a very common way of using EntroQ: stand up an EntroQ service, then
start up one or more workers responsible for handling the flow of tasks through
your system. Initial injection of tasks can easily be done with the provided
libraries or command-line clients.

### Commit Once, Maybe Work Twice

Tasks in EntroQ can only be *acknowledged* (deleted or moved) once. It is
possible for more than one claimant (or "worker") to be performing the same
tasks at the same time, but only one of them will succeed in deleting that
task. Because the other will fail to mutate its task, it will also know to
discard any work that it did.

Thus, it's possible for the actual work to be done more than once, but that
work will only be durably *recorded* once. Because of this, idempotent worker
design is still important, and some helpful principles are described below.

Meanwhile, to give some more detail about how two workers might end up working
on the same task, consider an "Early and Slow" (ES) worker and a "Late and
Quick" (LQ) worker. "Early and Slow" is the first to claim a particular task,
but then takes a long time getting it done. This delay may be due to a network
partition, or a slow disk on a machine, memory pressure, or process restarts.
Whatever the reason, ES claims a task, then doesn't acknowledge it before the
deadline.

While ES is busy working on its task, but not acknowleding or renewing it,
"Late and Quick" (LQ) successfully claims the task after its arrival time is
reached. If, while it is working on it, ES tries to commit its task, it has an
earlier version of the task and the commit fails. LQ then finishes and
succeeds, because it holds the most recent version of the task, which is the
one represented in the queue.

This also works if LQ finishes before ES does, in which case ES tries to
finally commit its work, but the task that is in the queue is no longer there:
it has been deleted because LQ finished it and requested deletion.

These semantics, where a claim is a mutating event, and every alteration to a
task changes its version, make it safe for multiple workers to attempt to do
the same work without the danger of it being committed (reported) twice.

### Safe Work

It is possible to abuse the ID/version distinction, by asking EntroQ to tell
you about a given task ID, then overriding the claimant ID and task version.
This is, in fact, how "force delete" works in the provided command-line client.
If you wish to mutate a task, you should have first _claimed_ it. Otherwise you
have no guarantee that the operation is safe. Merely reading it (e.g., using
the `Tasks` call) is not sufficient to guarantee safe mutation.

If you feel the need to use these facilities in normal worker code, however,
that should be a sign that the *design is wrong*. Only in extreme cases, like
manual intervention, should these ever be used. As a general rule,

    Only work on claimed tasks, and never override the claimant or version.

If you need to force something, you probably want to use the command-line
client, not a worker, and then you should be sure of the potential consequences
to your system.

To further ensure safety when using a competing-consumer work queue like
EntroQ, it is important to adhere to a few simple principles:

- All outside mutations should be idempotent, and
- Any output files should be uniquely named *every time*.

The first principle of idempotence allows things like database writes to be
done safely by multiple workers (remembering the ES vs. LQ concept above). As
an example, instead of *incrementing* a value in a database, it should simply
be *set to its final value*. Sometimes an increment is really what you want, in
which case you can make that operation idempotent by storing the final value
*in the task itself* so that the worker simply records that. That way, no
matter how many workers make the change, they make it to the same value. The
principle is this:

    Use stable, absolute values in mutations, not relative updates.

The second principle of unique file names applies to every worker that attempts
to write anything. Each worker, even those working on the same task, should
generate a random or time-based token in the file name for anything that it
writes to the file system. While this can generate garbage that needs to be
collected later, it also guarantees that partial writes do not corrupt complete
ones. File system semantics are quite different from database semantics, and
having uniquely-named outputs for every write helps to guarantee that
corruption is avoided.

In short, when there is any likelihood of a file being written by more than one
process,

    Choose garbage collection over write efficiency.

Thankfully, adhering to these safety principles is usually pretty easy,
resulting in great benefits to system stability and trustworthiness.

## Backends

To create a Go program that makes use of EntroQ, use the `entroq.New` function
and hand it a `BackendOpener`. An "opener" is a function that can, given a
context and suitable parameters, create a backend for the client to use as its
implementation.

To create a Python client, you can use the `entroq` package, which *always*
speaks gRPC to a backend EntroQ service.

In Go, there are three primary backend implementations:

- In-memory,
- PostgreSQL, and
- A gRPC client.

### In-Memory Backend

The `mem` backend allows your `EntroQ` instance to work completely in-process.
You can use exactly the same library calls to talk to it as you would if it
were somewhere on the network, making it easy to start in memory and progress
to persistent and/or networked implementations later as needed.

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
into a gRPC *client* that can talk to the proviced `qsvc` implementation,
described below.

This allows you to stand up a gRPC endpoint in front of your "real" persistent
backend, giving you authentication management and all of the other goodies that
gRPC provides on the server side, all exposed via protocol buffers and the
standard gRPC service interface.

All clients would then use the `grpc` backend to connect to this service, again
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

Unfortunately, CockroachDB does *not* contain support for the necessary SQL
statements, even though it speaks the PostgreSQL wire format. It cannot be used
in place of PostgreSQL without implementing an entirely new backend (not
impossible, just not done).

### Starting a PostgreSQL instance using Docker Compose

If you wish to start an EntroQ service backed by PostgreSQL, the easiest
approach is to use containers. A `docker-compose` example is given below that
also works wth `docker stack deploy`. Getting the same setup in Kubernetes is
also straightforward, provided that you have a PostgreSQL instance up.

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
in `proto`. Working services using various backends are found in the cmd
directories, e.g.,

- `cmd/eqmemsvc`
- `cmd/eqpgsvc`

You can build any of these and start them on desired ports and with desired
backend connections based on flag settings.

There is no *service* backend for `grpc`, though one could conceivably make
sense as a sort of proxy. But in that case you should really just use a
standard gRPC proxy.

When using one of these services, this is your main server and should be
treated as a singleton. Clients should use the `grpc` backend to connect to it.

Does making the server a singleton affect performance? Yes, of course, you can't
scale a singleton, but in practice if you are hitting a work queue that hard you
likely have a granularity problem in your design.

## Python Implementation

In `contrib/py` there is an implementation of a gRPC client in Python,
including a basic CLI. You can use this to talk to a gRPC EntroQ service, and
it includes a basic worker implementation so that you can relatively easily
replace uses of Celery in Python.

The Python implementation is made to be pip-installable directly from github:

```
$ python3 -m pip install git+https://github.com/shiblon/entroq
```

This creates the `entroq` module and supporting protocol buffers beneath it.
Use of these can be seen in the command-line client, implemented in
`__main__.py`.

## Examples

One of the most complete Go examples that is also used as a stress test is a
naive implementation of MapReduce, in `contrib/mr`. If you look in there, you
will see numerous idiomatic uses of the competing queue concept, complete with
workers, using queue empty tests a part of system semantics, queues assigned to
specific processes, and others.

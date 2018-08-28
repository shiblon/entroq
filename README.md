# EntroQ

A task queue with strong competing-consumer-friendly semantics and transactional updates.

## Concepts

There are essentially two operations that can be performed on a task store:

- Claim a random task that has "arrived", or
- Update tasks (delete, change, insert) atomically, optionally depending on other tasks.

There are a few read-only accessors, as well, but these are easier to
understand: they make a best effort attempt to show a consistent snapshot of
task queue state, but don't otherwise play into concurency. Occasionally you
will see the size of a queue queried when it is guaranteed that "empty" is
meaningful.

All mutating operations (including claims, which change the arrival time of a
task and modify its claimant) modify the task in such a way that, from the
perspective of another process that might also be trying to update it, that
task _no longer exists_. This guarantees "exactly one commit" semantics, although
it might not guarantee that the _work_ represented by a task is done exactly
once. Network partitions can cause work to happen more than once, but the
acknowledgement of that completion will only ever happen once.

A task, at its most basic, is defined by these traits:

- Queue Name
- Globally Unique ID+Version
- Arrival Time
- Value

Tasks also hold other information, such as the ID of the current client that
holds a claim on it, when it was created, and when it was modified, but these
are implementation details.

When claiming a task, the version is incremented and the arrival time is set to
a time in the future based on how it was claimed. Tasks with an arrival time
in the future are not available to be claimed by other clients.

Every modification to a task increments its version number, making it possible to
keep track of a task through many modifications by remembering its unique ID.
Any modification or dependency on a task, however, uses _both_ the ID and the
version, so that if one client holds an expired task, and another claims it,
the first cannot then delete or otherwise change it because the version number
no longer matches; the task no longer exists to change.

This allows for a robust competing-consumer worker setup, where workers are
guaranteed to not accidentally clobber each other's tasks.

## Backends

To create a client that can access the task manager, use the `entroq.New`
function and hand it a `BackendOpener`. An "opener" is a function that can,
given a context and suitable parameters, create a backend for the client to use
as its implementation.

### gRPC Backend

The grpc backend is somewhat special. It converts an `entroq.EntroQ` client
into a gRPC client, where the server is built from `qsvc`, described below.

This allows you to stand up a gRPC endpoint in front of your "real" persistent
backend, giving you authentication management and all of the other goodies that
gRPC provides on the server side, all exposed via protocol buffers and the
standard gRPC service interface.

All clients would then use the grpc backend to connect to this service, again
with gRPC authentication and all else that it provides. This is the preferred
way to use the EntroQ client library.

### PostgreSQL Backend

One of the backends is a PostgreSQL database. This is a nice backend option
because you can also move to a highly distributed database like CockroachDB
using the same wire format. Anything that speaks the Postgres wire format will
work, provided it implements the following SQL statements:

- `CREATE TABLE IF NOT EXISTS`
- `CREATE INDEX IF NOT EXISTS`
- `DISTINCT` in selects
- `UUID` type
- `BYTEA` type
- `TIMESTAMP WITH TIME ZONE` type
- `WITH ... UPDATE ... RETURNING` statements
- `BEGIN TRANSACTION ISOLATION LEVEL SERIALIZABLE`
- `SELECT ... FOR UPDATE`
- `INSERT INTO ... RETURNING`
- `UPDATE ... RETURNING`

Essentially, serializable transactions that also perform row-level locking by
way of SELECT FOR UPDATE are required, as well as the ability to specify that
modifying statements also return something with the RETURNING keyword.

### Starting a PostgreSQL instance in Docker

```
docker pull postgres
docker run --name pgsql -p 5432:5432 -e POSTGRES_PASSWORD=password -d postgres
```

To access it with a command line utility, you can do this:

```
docker run -it --rm --link pgsql:postgres postgres psql -h postgres -U postgres -d entroq
```

I usually like to create a new database and connect to that instead:

```
CREATE DATABASE entroq;
\c entroq
```

And then I use that database for the task client so as to not clobber standard postgres stuff.

Now you can access it from the host on its usual port.

## QSvc

The qsvc directory contains a grpc service implementation that exposes the
endpoints found in `qsvc/proto`. Working services using various backends are
found in the backend directories, e.g.,

- `pg/svc`
- `etcd/svc`

You can build any of these and start them on desired ports and with desired backend connections based on flag settings.

There is no backend for `grpc`, though one could conceivably make sense as a
sort of proxy. But in that case you should really just use a grpc proxy.

When using one of these services, this is your main server. Clients should use
the `grpc` backend to connect to it.

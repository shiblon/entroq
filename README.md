# EntroQ

A task queue with strong competing-consumer-friendly semantics and transactional updates.

## Concepts

There are essentially two operations that can be performed on a task store:

- Claim a random task that has "arrived", or
- Update tasks (delete, change, insert) atomically, optionally depending on other tasks.

All operations modify the task in such a way that, from the perspective of
another process that might also be trying to update it, that task no longer
exists. This guarantees "exactly one commit" semantics, although it might not
guarantee that the *work* represented by a task is done exactly once. Network
partitions can cause work to happen more than once, but the acknowledgement of
that completion will only ever happen once.

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
a time in the future, based on how it was claimed. Tasks with an arrival time
in the future are not available to be claimed by other clients.

Every modification to a task increments its version number, making it possible to
keep track of a task through many modifications by remembering its unique ID.
Any modification or dependency on a task, however, uses *both* the ID and the
version, so that if one client holds an expired task, and another claims it,
the first cannot then delete or otherwise change it because the version number
no longer matches.

This allows for a robust competing-consumer worker setup, where workers are
guaranteed to not accidentally clobber each other's tasks.

## Requirements

Any process can push tasks into the task store, which (currently) must be
backed by a PostgreSQL database.

Any database that speaks the Postgres wire protocol should work fine, provided
it supports the following SQL statements:

- CREATE EXTENSION "uuid-ossp"
- CREATE INDEX
- DISTINCT in selects
- UUID types
- WITH ... UPDATE ... RETURNING statements
- BEGIN TRANSACTION ISOLATION LEVEL SERIALIZABLE
- SELECT ... FOR UPDATE
- INSERT INTO ... RETURNING
- UPDATE ... RETURNING

Essentially, serializable transactions that also perform row-level locking by
way of SELECT FOR UPDATE are required, as well as the ability to specify that
modifying statements also return something with the RETURNING keyword.

This means, among other things, that CockroachDB should also work fine as a
backend, using the postgres driver (though the uuid-ossp extension may not be
available there, and the function to generate random UUIDs would change from
`uuid_generate_v4` to `gen_random_uuid` - there is some work to be done to make
this all compatible).

## Starting a PostgreSQL instance in Docker

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

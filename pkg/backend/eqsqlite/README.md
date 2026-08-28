# Experimental SQLite backend

`eqsqlite` is an experimental `entroq.Backend`. Its Go API, schema, and on-disk
format may change or be removed without a migration path. Do not treat an
`eqsqlite` database file as a stable interchange or archival format.

The backend uses WAL mode with `synchronous=FULL`. Reads use query-only
connections. Every claim, task/doc modification, doc claim, and GC mutation is
serialized through a single-connection `database/sql` write pool and committed
with `BEGIN IMMEDIATE`. Separate
backend instances and processes are still coordinated by SQLite's file locks.

Blocking claims use in-process `subq` notifications plus EntroQ's normal poll
interval. Notifications do not cross process boundaries and there is no
dedicated clock-trigger scanner: a future task becomes visible on the next poll
unless a modification through the same backend instance wakes the claimant first.
Empty blocking-claim polls use a read-only readiness probe and enter the write
pool only when a claim appears promising.

SQLite remains a single-writer database. The backend does not emulate
`SKIP LOCKED`, shard tables, or promise write scalability from adding workers.
The claim query chooses randomly from the first 64 ready tasks in arrival order;
this reduces deterministic contention while keeping selection close to the
head of the queue.

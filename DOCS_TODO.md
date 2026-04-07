# Documentation TODO

This is a master tracking document for the upcoming documentation polish pass. The actual writing will be handled by the USER.

## Status Checklist
- [ ] Refine Operator's Guide
- [ ] Refine Notification Principles
- [ ] **NEW**: Document gRPC Load Balancing Capabilities
- [ ] Finalize README integration

---

## 1. Operator's Guide (Source: deploy/OPERATOR_GUIDE.md)

# EntroQ Operator's Guide: Invariants and Optimization for PostgreSQL backends

EntroQ, whether backed by in-memory or PostgreSQL implementations, is built on two core pillars that remain **Absolute Invariants** regardless of your configuration, deployment scale, or choice of client. For PostgreSQL, SQL transactions are fundamental to the way this works, and also enable the ability to carry out *additional* database operations within the same transaction as task management.

1.  **Atomic Correctness**: Every `Claim` and `Modify` is a version-locked (SQL in the case of PostgreSQL) transaction. The system guarantees that tasks are never dropped or double-committed, even in high-concurrency environments.

2.  **Guaranteed Progress**: All workers maintain an internal polling safety net. Even if the notification bus is completely disabled, work will always be found and processed eventually. That safety net can have a cost - if you are seeing load on systems with many workers but few tasks, reduce the number of workers, or increase poll intervals.

## The Performance Decision: Heartbeat or No?

A heartbeat is a periodic request for which queues have recently seen tasks become available due to the passage of time. Regardless of whether you enable a heartbeat or not, notifications will happen when a task is inserted or modified.

If you do enable a heartbeat, then within the heartbeat interval, the system will detect which queues to notify on when tasks within them have become available because of the passage of time. This is a "best effort" optimization - if a heartbeat is missed, the next heartbeat will catch up.

Heartbeats are grouped and the notifications are minimized to happen only once per heartbeat interval. Thus, only one worker really needs to issue a heartbeat, and all workers will benefit from the notifications.

How many heartbeat workers should be deployed? In most cases all or none is just fine. If you have a lot of workers, you might consider firing up a single worker that listens on a single empty queue, and issues a heartbeat, while the rest don't have to have a heartbeat configured at all. This is the pattern followed for gRPC: the service that is started is a heartbeat client, and it handles gRPC requests. All gRPC workers have no heartbeat, but are notified through the gateway.

In this case, you don't really need to think too hard about how heartbeats work. You just fire up a server, and you fire up clients, and by virtue of their server of client roles, they do the right thing.

In all cases, if there is no heartbeat, you will likely be fine regardless. Things just work, and latency in the common case (task insertion should be handled immediately by workers) will be very small. *Heartbeats only optimize for the passage of time, not for insertion or modification events, because those are already fine.*

| Deployment Role | Performance Profile | DB Interaction | Latency for Scheduled Work |
| :--- | :--- | :--- | :--- |
| **Silent Worker** | High-Scale / Passive | REACTIVE (Zero Noise) | Polling Interval (e.g., 30-60s) |
| **Active Hub** | Performance / Leader | PROACTIVE (Periodic Tick) | Sub-Second |
| **Singleton Gateway** | Optimized Solo | HYBRID (Ticker Bridge) | Sub-Second |

## 1. The Heart Hub (Leader/Gateway)
For most deployments, the gRPC gateway (`eqpg serve`) is a natural candidate for enabling a heartbeat if periodic readiness updates are desired. 

- **Efficient Leadership**: The gateway emits a heartbeat every 5s to move the database watermark and broadcast readiness notifications to the cluster.
- **Connection Optimization**: Since the gateway holds its own "Ticker-to-Local Bridge," it can safely use `--no_listen=true` to save a persistent PostgreSQL connection while still maintaining sub-millisecond local wake-ups for work it inserts itself.

## 2. The Silent Worker (Scale)
General-purpose workers by default emit no heartbeat. They establish a single, shared `LISTEN` connection on the PostgreSQL bus and only wake up when a notification occurs. When these workers are gRPC clients, the notification happens with no special configuration. When workers are direct-to-postgres clients, they will only receive immediate notifications if a task is inserted or modified, unless at least one of them emits a heartbeat.

## 3. High-Performance "Flaring"
If you have a high-traffic system that requires sub-second task arrival for remote workers, reduce the heartbeat on your Hub:
`--heartbeat=500ms`

> [!WARNING]
> While high-frequency heartbeats improve responsiveness, they increase the transaction rate on your database. Use the `p_min_interval` setting in the PostgreSQL schema to protect yourself from misconfigurations.

## Troubleshooting: The "Passive" Cluster
If you see tasks sitting in the `Ready` state (meaning their `At` time has passed), but workers are not claiming them for 30-60 seconds, you likely have a **Passive Cluster** (Zero Hearts).

**The Reality**:
- **Immediate work** (tasks ready upon insertion) still triggers clusters via `pg_notify`.
- **Scheduled work** (tasks with a future arrival time) must wait for a worker to poll.
- **Fix**: Designate one `eqpg serve` instance as a Heart using `--heartbeat=5s`. This "notifies" the cluster the moment scheduled work becomes ready.

A useful recipe for a single-heartbeat client is to do the following:

- Run a lightweight sidecar on the postgres database itself
- The sidecar is a postgres client with a non-zero heartbeat duration

To create the sidecar, you have multiple options.

TODO: flesh out a recipe for heartbeats. A single config task in a single queue. The sidecar modifies that task every few seconds, simply advancing its arrival time. In a background process, it issues its heartbeat. If ever the AT advancement fails, the config has changed and the heartbeat process reads it again to find out whether to change its behavior.

## Reference: Flag Meanings
- **`--heartbeat`**: How often this node should move the database watermark. 0 = Never (Passive).
- **`--no_listen`**: Disables the persistent `LISTEN` connection. Use this for singletons that act as the only entry point for their local workers.

---

## 2. Notification Principles (Source: notification_principles.md)

# EntroQ Principles: Invariants and Notifications

EntroQ defines a set of **Fundamental Invariants** that are guaranteed regardless of your architecture, client language, or notification setup:

1.  **Safety**: Every `Claim` and `Modify` is an atomic, version-locked SQL transaction. It is mathematically impossible to double-claim a task or drop a commit once the transaction succeeds.
2.  **Liveness**: Every client maintains a polling safety net. Even if the notification bus is completely silent, progress will always be made.

The notification system described below is a **Progressive Enhancement**. It primarily leverages PostgreSQL's `LISTEN/NOTIFY` to reduce latency for high-precision distributed workloads without compromising the system's core reliability.

## 1. User Intuition and Expectations

When a user or a high-level application performs a manual `Modify` or `Insert`, the intuition is that action should be immediate. If a task is ready now, it should be handled now. Any perceptible delay between the moment of insertion and the start of processing feels like a system failure. This expectation remains even for scheduled work--tasks with a future `At` time are expected to become available with cron-like precision. While we cannot always guarantee an instantaneous response in every distributed environment, the system is designed to minimize this gap through a combination of proactive notifications and reactive bridges.

## 2. Topologies and Transitions

EntroQ clusters adapt to their environment. In a singleton setup, a single Go-based gateway like `eqpgsvc` might manage all connections, using an internal **Ticker-to-Local Bridge** to fan out notifications to local workers without even touching the PostgreSQL `LISTEN` bus. This resulting sub-millisecond wake-up is effectively "free" from a database perspective.

As you scale out to distributed workers, they transition into a **Quiet-by-Default** profile. They establish a single `LISTEN` connection but perform zero background database activity, essentially waiting for a designated **Heart Hub** to launch a notification once scheduled work becomes ready. Even if you are forced to use a connection pooler like PgBouncer in transaction mode--where `LISTEN/NOTIFY` is often unsupported--the system elegantly degrades to polling. You lose some sub-second precision, but you never lose correctness.

## 3. Immediate Bridging vs. Heartbeat Notifications

We handle task readiness in two distinct layers to keep the database quiet. The first is the **Redundant Local Bridge**: when a worker or gateway successfully modifies a task, it immediately signals all local handlers within the same process. This bypasses the network and the database entirely, ensuring that if work is ready, it is handled.

The second layer is the **Heartbeat Notification**. This is emitted by a leader node via a background ticker, moving the database's internal watermark and notifying the rest of the cluster for work that transitioned from "future" to "ready."

## 4. SubQ and the Thundering Herd

A common worry in notification-heavy systems is the "thundering herd," where every worker wakes up simultaneously to fight over a single task. EntroQ mitigates this in two places. Locally, the `subq` library ensures that only one worker goroutine is woken up for a given signal on a channel. Notifiers are non-blocking; the signal is a "best effort" nudge that is safely dropped if no one is listening.

When using gRPC clients, whether through the JSON endpoint or the gRPC endpoint, clients that run workers automatically benefit: the "goroutine" mentioned above is residing on the server. When the server handler receives and responds to a request from a remote worker, the goroutine in which that handler runs is the one that is waiting and one of the workers on its queue that wakes up to receive a notification.

In case there are multiple workers connected directly to a postgres instance, the notification is a broadcast because it uses pg_notify. It is anticipated that this is generally not a big deal, because notification only works when not sitting behind a connection pool, so the number of receivers of the broadcast will always be small (e.g., 50 to 100) because of Postgres's inherent connection limitations.

The workers that do wake up attempt a claim via a SKIP LOCKED query, so they are still going to be contention-avoidant, even in imperfect operating conditions.

## 5. Thin Clients and Protocol Simplicity

The protocol is simple enough for humans and thin clients alike. A developer can run `LISTEN q_myqueue;` in a standard `psql` shell and see the notifications as they happen. This transparency is a feature: whether you are writing a complex Go worker or a simple Bash script using `psql`, the same invariants of safety and progress apply. Correctness is enforced by SQL transactions, while notifications remain a simple, progressive enhancement to your system's performance.

The `eqc` binary is a command-line tool that can be used to do nearly everything that a custom worker can do, even respond to notifications when it is used as a gRPC client (the typical case). You may use it to do surgery to your queues, to dump stats, and to generally poke at the system to see what it is doing. It is  powerful and simple operational tool for responding to issues in the system quickly.

---

## 3. NEW: gRPC Load Balancing (To be written)

### Key Points to Include:
- **Architectural Unlock**: With the `PGNotifyWaiter` and unified heartbeat/flusher logic, multiple gRPC server instances can now listen to the same Postgres backend efficiently.
- **Client Transparency**: Standard gRPC load balancers (Envoy, Nginx, K8s Service) can distribute traffic across $N$ servers.
- **Failover**: `pq.Listener` automatically re-establishes `LISTEN` connections if a server reboots or the network blips.
- **Horizontal Scaling**: Scale up to ~10 servers comfortably before hitting Postgres connection limits or "thundering herd" overhead on notifications.
- **Designated Hearts**: Mention the recommendation to only enable `--heartbeat` on 1 or 2 nodes in the balanced cluster to keep DB noise low.

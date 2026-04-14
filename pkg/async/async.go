// Package async provides primitives for building asynchronous HTTP networking
// over EntroQ task queues. Services communicate by sending and receiving tasks
// rather than making direct HTTP connections, gaining fault tolerance and
// decoupled addressing without changing their HTTP interface.
//
// # Sidecar Pattern (Single EQ Instance)
//
// The most helpful deployment for those who have preexisting microservices, or
// are working in a constrained environment where it is not reasonable to
// retool existing services, is a sidecar pair sharing one EntroQ instance. The
// Sender translates outgoing HTTP calls from a local service into Envelope
// tasks on a named queue. One or more Receiver workers claim those tasks,
// forward them as HTTP requests to an upstream service, and enqueue a Response
// task on the per-request response queue. The sender unblocks and returns the
// response to the original caller.
//
//	[Service A] -HTTP-> [Sender] -task-> [EQ] <-claim- [Receiver] -HTTP-> [Service B]
//	                       ^                                  |
//	                       +--------response task-------------+
//
// This permits basic microservices to communicate with one another through the
// queueing system without knowing they are part of such a system. The services
// themselves are synchronous, but only with local connections in their
// container. The rest of the system is fully asynchronous.
//
// This works across datacenters if the remote receiver can reach EQ over the
// WAN. mTLS (--cert/--key/--ca flags on eqlink) is used to authenticate the
// connection and, when configured, to pass the caller's identity to EntroQ for
// authorization.
//
// # Cross-Instance Forwarding (Two EQ Instances)
//
// When each datacenter needs its own EQ instance for fault isolation, a
// forwarder worker bridges the two:
//
//	[EQ-A] <-claim- [Forwarder] -insert-> [EQ-B]
//
// Turning the above approach upside-down, you can have a queue represent
// sending work to a remote entroq system, and similarly for receiving from
// such a system. The same linkage works: a "send to remote" queue is listened
// to by a receiver, which then forwards a request to the remote EntroQ
// (perhaps in a different datacenter). A sender waits for connection and
// converts data into messages to place into a local queue.
//
// Delivery is at-least-once: the insert into EQ-B and the delete from EQ-A
// are separate transactions. If the forwarder crashes between them the task
// will be re-forwarded. Workers on EQ-B should be designed for idempotent
// processing.
//
// Response routing across instances requires additional complexity. For
// request-response patterns across DCs, the single-EQ approach (receiver
// connects to EQ-A over WAN) is simpler. The two-instance forwarder is best
// suited for one-way task delegation: fire work at a remote DC and let it
// process independently.
package async

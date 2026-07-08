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
// # Cross-Instance Handoff (Two EQ Instances)
//
// When each datacenter needs its own EQ instance, moving tasks between instances
// is a separate concern handled by the pull worker (package
// github.com/shiblon/entroq/pkg/workers/handoffworker, exposed as "eqlink pull"):
// it claims from a queue on a source instance and delivers into an inbox on the
// destination, exactly once in effect. This package is the sender/receiver
// sidecar; for request-response across datacenters the single-EQ approach (the
// receiver connects to the remote EQ over the WAN) remains the simpler option.
package async

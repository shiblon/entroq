// Package async provides experimental primitives for carrying HTTP exchanges
// over EntroQ task queues. Its wire protocol may change without compatibility.
// Services communicate by sending and receiving tasks rather than making
// direct cross-service HTTP connections, gaining decoupled addressing and
// queue-based load distribution without changing their HTTP interface.
//
// # Sidecar Pattern (Single EQ Instance)
//
// The most helpful deployment for those who have preexisting microservices, or
// are working in a constrained environment where it is not reasonable to
// retool existing services, is a sidecar pair sharing one EntroQ instance. The
// Sender translates outgoing HTTP calls from a local service into Envelope
// tasks on a named queue. One or more Receiver workers claim those tasks,
// forward them as HTTP requests to an upstream service, and enqueue Response
// tasks on ephemeral lanes. The sender relays response metadata and body bytes
// to the original caller as frames arrive.
//
//	[Service A] <-HTTP-> [Sender] <-tasks-> [EQ] <-tasks-> [Receiver] <-HTTP-> [Service B]
//
// This permits basic microservices to communicate with one another through the
// queueing system without knowing they are part of such a system. The services
// themselves hold only local HTTP connections in their container. EQLink uses
// two independent stop-and-wait lane pairs over queues, one for each HTTP body
// direction. Empty Envelope or Response frames acknowledge data in the same
// lane; there is no distinct acknowledgement frame type. This carries arbitrary
// byte segments without parsing SSE, NDJSON, HTTP chunks, or gRPC messages and
// permits concurrent HTTP/2 request and response streaming. HTTP protocol
// upgrades such as WebSocket are not supported.
//
// Every segment waits for an EntroQ round trip before the next segment in that
// direction. EQLink streaming is therefore intended for low-rate interactions
// such as status, heartbeats, and compatibility with otherwise unsupported
// client languages, not high-throughput streaming.
//
// After one minute without a frame to send, the side that owns a lane turn
// sends an ordinary empty frame. Only a frame received from the peer resets the
// session liveness deadline; three minutes of peer silence ends the exchange.
// Both durations scale together when the request timeout is configured.
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

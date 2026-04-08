// Package async provides primitives for building asynchronous HTTP networking
// over EntroQ task queues. Services communicate by sending and receiving tasks
// rather than making direct HTTP connections, gaining fault tolerance and
// decoupled addressing without changing their HTTP interface.
package async

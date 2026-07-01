// Command eqlink is an async HTTP networking sidecar built on EntroQ.
// Services communicate through task queues rather than direct connections,
// gaining fault tolerance and load balancing without changing their HTTP code.
//
// Run "eqlink run" to start the full sidecar (sender + receiver + GC).
// Individual components can be run separately with "eqlink send", "eqlink recv",
// and "eqlink gc".
//
// "eqlink pull" and "eqlink push" link two EntroQ instances directly, moving
// tasks between them exactly once (pull claims from a remote instance into a
// local inbox; push claims from a local queue into a remote inbox).
package main

import "github.com/shiblon/entroq/cmd/eqlink/cmd"

func main() {
	cmd.Execute()
}

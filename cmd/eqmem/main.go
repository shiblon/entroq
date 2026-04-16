// Command eqmem is the in-memory EntroQ management tool.
// Run "eqmem serve" to start the service.
// Future subcommands will expose journal inspection utilities.
package main

import "github.com/shiblon/entroq/cmd/eqmem/cmd"

func main() {
	cmd.Execute()
}

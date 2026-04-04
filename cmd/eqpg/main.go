// Command eqpg is the PostgreSQL-backed EntroQ management tool.
// Run "eqpg serve" to start the service, "eqpg schema" for schema utilities.
package main

import "github.com/shiblon/entroq/cmd/eqpg/cmd"

func main() {
	cmd.Execute()
}

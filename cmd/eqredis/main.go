// Command eqredis is the Redis-backed EntroQ management tool.
// Run "eqredis serve" to start the service.
package main

import "github.com/shiblon/entroq/cmd/eqredis/cmd"

func main() {
	cmd.Execute()
}

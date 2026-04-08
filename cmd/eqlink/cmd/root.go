package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	entroqAddr string
)

var rootCmd = &cobra.Command{
	Use:   "eqlink",
	Short: "Async HTTP networking sidecar over EntroQ task queues.",
	Long: `eqlink translates synchronous HTTP calls into EntroQ task queue operations,
giving services fault tolerance and load balancing without changing their code.

The sender proxies outgoing HTTP calls from the local service into queues.
The receiver claims tasks from queues and forwards them to the local service.
The GC cleans up stale response queues left by crashed senders.`,
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&entroqAddr, "entroq", "localhost:37706", "EntroQ gRPC service address.")
}

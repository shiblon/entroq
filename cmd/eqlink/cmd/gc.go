package cmd

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/gc"
	"github.com/spf13/cobra"
)

var (
	gcInterval  time.Duration
	gcQueueRoot string
)

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Run the global GC: cleans up stale response queues left by crashed or removed sidecars.",
	Long: `Periodically scans queues under the given prefix for a garbage-collection
directive in the name (a /gc=<timestamp> component) whose time has passed, and
deletes the claimable tasks in them. Async response queues
(<root>/*/response/gc=<timestamp>) are the primary case.

Run one instance of this command per EntroQ deployment for global coverage.
Each "eqlink run" sidecar also runs a local GC scoped to its own queue.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		eq, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, eqgrpc.WithInsecure()))
		if err != nil {
			return fmt.Errorf("gc connect: %w", err)
		}
		defer eq.Close()

		return gc.RunLoop(ctx, eq,
			gc.WithMatch(entroq.MatchPrefix(gcQueueRoot)),
			gc.WithInterval(gcInterval),
		)
	},
}

func init() {
	flags := gcCmd.Flags()
	flags.DurationVar(&gcInterval, "interval", 10*time.Minute, "How often to run the GC scan.")
	flags.StringVar(&gcQueueRoot, "root", "/", "Queue name prefix to scan.")

	rootCmd.AddCommand(gcCmd)
}

package cmd

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/spf13/cobra"
)

var (
	gcInterval  time.Duration
	gcGrace     time.Duration
	gcQueueRoot string
)

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Run the global GC: cleans up stale response queues left by crashed or removed sidecars.",
	Long: `Periodically scans for stale response queues (<root>/*/response/exp=<timestamp>/*)
where the expiry timestamp has passed (plus a grace period), and deletes any
tasks still in those queues.

Run one instance of this command per EntroQ deployment for global coverage.
Each "eqlink run" sidecar also runs a local GC scoped to its own queue.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		eq, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, eqgrpc.WithInsecure()))
		if err != nil {
			return fmt.Errorf("gc connect: %w", err)
		}
		defer eq.Close()

		return async.RunGCLoop(ctx, eq, gcQueueRoot,
			async.WithGCInterval(gcInterval),
			async.WithGCGrace(gcGrace),
		)
	},
}

func init() {
	flags := gcCmd.Flags()
	flags.DurationVar(&gcInterval, "interval", 10*time.Minute, "How often to run the GC scan.")
	flags.DurationVar(&gcGrace, "grace", 15*time.Second, "Extra time after expiry before GC deletes a response queue.")
	flags.StringVar(&gcQueueRoot, "root", "/", "Queue name prefix root to scan.")

	rootCmd.AddCommand(gcCmd)
}

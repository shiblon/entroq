package cmd

import (
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq/pkg/workers/handoffworker"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	handoffFrom          string
	handoffFromCert      string
	handoffFromKey       string
	handoffFromCA        string
	handoffFromTokenFile string
	handoffTo            string
	handoffToCert        string
	handoffToKey         string
	handoffToCA          string
	handoffToTokenFile   string
	handoffFromQueues    []string
	handoffToQueue       string
	handoffFromName      string
	handoffTTL           time.Duration
	handoffConcurrency   int
)

var handoffCmd = &cobra.Command{
	Use:   "handoff",
	Short: "Hand tasks off from one EntroQ instance to another, exactly once.",
	Long: `Claims tasks from --from-queue on the --from instance and delivers them into
--to-queue on the --to instance, exactly once in effect. Each delivery atomically
inserts the inbox task and a dedup tombstone on the destination, then deletes the
source task; a crash that re-delivers collides on the tombstone, so no duplicate
inbox task is produced.

Data always flows from --from to --to. "Push" and "pull" are only where you run
this: next to --from it reads like pushing work upstream, next to --to like
pulling it down. Run it wherever keeps the busy leg off the wire. Both endpoints
are configured symmetrically, so neither is privileged.

Each endpoint has its own transport security (--from-cert/-key/-ca and
--to-cert/-key/-ca) and optional bearer-token auth (--from-authz-token-file,
--to-authz-token-file, reloaded on rotation). The root --entroq/--cert/etc. flags
are not used by handoff.

--from-name is mixed into the deterministic transfer id that dedups
redeliveries. It defaults to --from, which is correct for a single source. When
MULTIPLE distinct sources hand off into the same --to-queue, each MUST set a
unique, stable --from-name (a tenant or host id); sharing a name lets their tasks
collide on the destination and one source's work is silently dropped as a
duplicate.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Cancel on SIGINT/SIGTERM: workers stop claiming and exit cleanly. An
		// interrupted in-flight delivery is safe -- the source task's lease keeps
		// it, and the next claim re-delivers idempotently (the tombstone dedups).
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		g, gCtx := errgroup.WithContext(ctx)

		from, err := openEntroq(gCtx, g, "from", handoffFrom, handoffFromCert, handoffFromKey, handoffFromCA, handoffFromTokenFile)
		if err != nil {
			return fmt.Errorf("from entroq: %w", err)
		}
		defer from.Close()

		to, err := openEntroq(gCtx, g, "to", handoffTo, handoffToCert, handoffToKey, handoffToCA, handoffToTokenFile)
		if err != nil {
			return fmt.Errorf("to entroq: %w", err)
		}
		defer to.Close()

		fromName := handoffFromName
		if fromName == "" {
			fromName = handoffFrom
		}

		for range handoffConcurrency {
			g.Go(func() error {
				return handoffworker.Run(gCtx, from,
					handoffworker.WithDest(to),
					handoffworker.WithInbox(handoffToQueue),
					handoffworker.WithQueues(handoffFromQueues...),
					handoffworker.WithTTL(handoffTTL),
					handoffworker.WithSource(fromName),
				)
			})
		}

		return g.Wait()
	},
}

func init() {
	flags := handoffCmd.Flags()
	flags.StringVar(&handoffFrom, "from", "", "Source EntroQ gRPC address to claim tasks from (required).")
	flags.StringVar(&handoffFromCert, "from-cert", "", "TLS certificate for the --from connection.")
	flags.StringVar(&handoffFromKey, "from-key", "", "TLS private key for the --from connection.")
	flags.StringVar(&handoffFromCA, "from-ca", "", "CA bundle for verifying the --from peer.")
	flags.StringVar(&handoffFromTokenFile, "from-authz-token-file", "", "Bearer token file for the --from connection (reloaded on rotation).")
	flags.StringVar(&handoffTo, "to", "", "Destination EntroQ gRPC address to deliver tasks to (required).")
	flags.StringVar(&handoffToCert, "to-cert", "", "TLS certificate for the --to connection.")
	flags.StringVar(&handoffToKey, "to-key", "", "TLS private key for the --to connection.")
	flags.StringVar(&handoffToCA, "to-ca", "", "CA bundle for verifying the --to peer.")
	flags.StringVar(&handoffToTokenFile, "to-authz-token-file", "", "Bearer token file for the --to connection (reloaded on rotation).")
	flags.StringSliceVar(&handoffFromQueues, "from-queue", nil, "Source queue(s) on --from to claim tasks from (required, repeatable).")
	flags.StringVar(&handoffToQueue, "to-queue", "", "Destination inbox queue on --to (required).")
	flags.StringVar(&handoffFromName, "from-name", "", "Stable source identifier mixed into the transfer id (defaults to --from). MUST be unique per source when several sources feed one --to-queue.")
	flags.DurationVar(&handoffTTL, "ttl", handoffworker.DefaultTTL, "Tombstone retention window; must exceed worst-case recovery time.")
	flags.IntVar(&handoffConcurrency, "concurrency", 1, "Number of concurrent handoff workers.")

	handoffCmd.MarkFlagRequired("from")
	handoffCmd.MarkFlagRequired("to")
	handoffCmd.MarkFlagRequired("from-queue")
	handoffCmd.MarkFlagRequired("to-queue")

	rootCmd.AddCommand(handoffCmd)
}

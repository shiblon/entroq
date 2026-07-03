package cmd

import (
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/gc"
	"github.com/shiblon/entroq/pkg/workers/pullworker"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	destEntroqAddr   string
	destCertFile     string
	destKeyFile      string
	destCAFile       string
	pushSourceQueues []string
	pushInbox        string
	pushTTL          time.Duration
	pushSourceName   string
	pushConcurrency  int
	pushRunGC        bool
)

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push tasks from a local queue into a remote EntroQ instance's inbox, exactly once.",
	Long: `Claims tasks from --source-queue on the local instance (--entroq) and delivers
them into --inbox on a remote EntroQ instance (--dest-entroq), exactly once in
effect. This is the mirror of "eqlink pull": same handoff, but run next to the
SOURCE. Use it for hub-and-spoke fan-in, where each leaf pushes its work up to a
central instance.

The dedup tombstone is created on the destination, so with push it lives on the
remote instance: its eager cleanup crosses the wire, and its crash orphans are
reaped by the remote server's built-in GC (the tombstone queue carries a gc=
marker), not by this process. This requires the remote destination server to run
GC, which is the default; if it does not (e.g. a direct-to-PostgreSQL instance),
pass --run-gc to reap tombstones from here across the wire.

--source-name MUST be unique per source instance. It is mixed into the
deterministic transfer id; if two leaves share a name, their tasks can collide on
the shared destination and one leaf's work would be silently dropped as a
duplicate. Use the tenant id, host id, or similar stable unique value.

The local (--entroq) connection secures with --cert/--key/--ca and authenticates
with --authz-token-file; the remote destination uses --dest-cert/--dest-key/--dest-ca,
falling back to --cert/--key/--ca when none of those are set.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Cancel on SIGINT/SIGTERM: workers stop claiming and exit cleanly. An
		// interrupted in-flight delivery is safe -- the source task's lease keeps
		// it, and the next claim re-delivers idempotently (the tombstone dedups).
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		g, gCtx := errgroup.WithContext(ctx)

		// Local (source) instance: where tasks are claimed from. Transport
		// security and auth come from --cert/--key/--ca and --authz-token-file.
		src, err := localEQ(gCtx, g)
		if err != nil {
			return fmt.Errorf("source entroq: %w", err)
		}
		defer src.Close()

		// Remote (destination) instance: where tasks are delivered. Its TLS falls
		// back to the local --cert/--key/--ca when no --dest-* flags are set.
		destTLS, err := remoteTLS("dest", destCertFile, destKeyFile, destCAFile)
		if err != nil {
			return fmt.Errorf("dest tls: %w", err)
		}
		var destOpts []eqgrpc.Option
		if destTLS != nil {
			destOpts = append(destOpts, eqgrpc.WithDialOpts(grpc.WithTransportCredentials(credentials.NewTLS(destTLS))))
		} else {
			destOpts = append(destOpts, eqgrpc.WithInsecure())
		}
		dst, err := entroq.New(gCtx, eqgrpc.Opener(destEntroqAddr, destOpts...))
		if err != nil {
			return fmt.Errorf("dest entroq: %w", err)
		}
		defer dst.Close()

		// Push workers: claim from the local source, deliver into the remote inbox.
		for range pushConcurrency {
			g.Go(func() error {
				return pullworker.Run(gCtx, src,
					pullworker.WithDest(dst),
					pullworker.WithInbox(pushInbox),
					pullworker.WithQueues(pushSourceQueues...),
					pullworker.WithTTL(pushTTL),
					pullworker.WithSource(pushSourceName),
				)
			})
		}

		if pushRunGC {
			// Fallback for a destination without built-in GC (e.g. a direct-to-
			// PostgreSQL instance): reap this inbox's tombstones on the remote.
			g.Go(func() error {
				return gc.RunLoop(gCtx, dst, gc.WithMatch(entroq.MatchExact(pullworker.TombstoneQueue(pushInbox))))
			})
		}

		return g.Wait()
	},
}

func init() {
	flags := pushCmd.Flags()
	flags.StringVar(&destEntroqAddr, "dest-entroq", "", "Remote destination EntroQ gRPC address to deliver to (required).")
	flags.StringVar(&destCertFile, "dest-cert", "", "TLS certificate for the destination connection.")
	flags.StringVar(&destKeyFile, "dest-key", "", "TLS private key for the destination connection.")
	flags.StringVar(&destCAFile, "dest-ca", "", "CA bundle for verifying the destination peer.")
	flags.StringSliceVar(&pushSourceQueues, "source-queue", nil, "Local queue(s) to claim tasks from (required, repeatable).")
	flags.StringVar(&pushInbox, "inbox", "", "Remote destination inbox queue (required).")
	flags.DurationVar(&pushTTL, "ttl", pullworker.DefaultTTL, "Tombstone retention window; must exceed worst-case recovery time.")
	flags.StringVar(&pushSourceName, "source-name", "", "Stable identifier for this source instance, unique across all pushers to the same destination (required).")
	flags.IntVar(&pushConcurrency, "concurrency", 1, "Number of concurrent push workers.")
	flags.BoolVar(&pushRunGC, "run-gc", false, "Reap this inbox's dedup tombstones with a local GC loop against the remote destination. Off by default: the destination server collects them. Enable when the remote has no built-in GC (e.g. a direct-to-PostgreSQL instance, or a server run with --no_gc).")

	pushCmd.MarkFlagRequired("dest-entroq")
	pushCmd.MarkFlagRequired("source-queue")
	pushCmd.MarkFlagRequired("inbox")
	pushCmd.MarkFlagRequired("source-name")

	rootCmd.AddCommand(pushCmd)
}

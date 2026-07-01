package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/worker"
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
remote instance: eager cleanup and the tombstone reaper cross the wire. That is
the inherent cost of running next to the source rather than the destination; for
a busy fan-in you may prefer to run a single reaper next to the hub instead of
relying on each leaf's remote reaping.

--source-name MUST be unique per source instance. It is mixed into the
deterministic transfer id; if two leaves share a name, their tasks can collide on
the shared destination and one leaf's work would be silently dropped as a
duplicate. Use the tenant id, host id, or similar stable unique value.

The local (--entroq) connection secures with --cert/--key/--ca and authenticates
with --authz-token-file; the remote destination uses --dest-cert/--dest-key/--dest-ca.`,
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

		// Remote (destination) instance: where tasks are delivered.
		destTLS, err := loadTLSConfig(destCertFile, destKeyFile, destCAFile)
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

		// Reaper: delete spent tombstones from the remote instance once due. This
		// crosses the wire; see the note about a hub-side reaper for busy fan-in.
		g.Go(func() error {
			reaper := worker.New(dst, worker.WithDoModify(
				func(_ context.Context, t *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
					return []entroq.ModifyArg{t.Delete()}, nil
				}))
			return reaper.Run(gCtx, worker.Watching(pullworker.TombstoneQueue(pushInbox)))
		})

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

	pushCmd.MarkFlagRequired("dest-entroq")
	pushCmd.MarkFlagRequired("source-queue")
	pushCmd.MarkFlagRequired("inbox")
	pushCmd.MarkFlagRequired("source-name")

	rootCmd.AddCommand(pushCmd)
}

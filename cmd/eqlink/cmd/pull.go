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
	sourceEntroqAddr string
	sourceCertFile   string
	sourceKeyFile    string
	sourceCAFile     string
	pullSourceQueues []string
	pullInbox        string
	pullTTL          time.Duration
	pullSourceName   string
	pullConcurrency  int
)

var pullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull tasks from a remote EntroQ instance into a local inbox, exactly once.",
	Long: `Claims tasks from --source-queue on a remote EntroQ instance (--source-entroq)
and delivers them into --inbox on the local instance (--entroq), exactly once in
effect. Each delivery atomically inserts the inbox task and a dedup tombstone on
the local instance, then deletes the source task. A crash that re-delivers
collides on the tombstone, so no duplicate inbox task is produced.

Run this next to the destination instance: only the claim from the source crosses
the wire. A reaper deletes spent tombstones from <inbox>/_tombstone once their TTL
elapses; the happy path deletes its own tombstone immediately, so the reaper only
handles crash orphans.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Cancel on SIGINT/SIGTERM: workers stop claiming and exit cleanly. An
		// interrupted in-flight delivery is safe -- the source task's lease keeps
		// it, and the next claim re-delivers idempotently (the tombstone dedups).
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		// Local (destination) instance: where tasks are delivered.
		dst, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, eqgrpc.WithInsecure()))
		if err != nil {
			return fmt.Errorf("dest entroq: %w", err)
		}
		defer dst.Close()

		// Remote (source) instance: where tasks are claimed from.
		srcTLS, err := loadTLSConfig(sourceCertFile, sourceKeyFile, sourceCAFile)
		if err != nil {
			return fmt.Errorf("source tls: %w", err)
		}
		var srcOpts []eqgrpc.Option
		if srcTLS != nil {
			srcOpts = append(srcOpts, eqgrpc.WithDialOpts(grpc.WithTransportCredentials(credentials.NewTLS(srcTLS))))
		} else {
			srcOpts = append(srcOpts, eqgrpc.WithInsecure())
		}
		src, err := entroq.New(ctx, eqgrpc.Opener(sourceEntroqAddr, srcOpts...))
		if err != nil {
			return fmt.Errorf("source entroq: %w", err)
		}
		defer src.Close()

		sourceName := pullSourceName
		if sourceName == "" {
			sourceName = sourceEntroqAddr
		}

		g, gCtx := errgroup.WithContext(ctx)

		// Pull workers: claim from the source, deliver into the local inbox.
		for range pullConcurrency {
			g.Go(func() error {
				return pullworker.Run(gCtx, src,
					pullworker.WithDest(dst),
					pullworker.WithInbox(pullInbox),
					pullworker.WithQueues(pullSourceQueues...),
					pullworker.WithTTL(pullTTL),
					pullworker.WithSource(sourceName),
				)
			})
		}

		// Reaper: delete spent tombstones from the local instance once due.
		g.Go(func() error {
			reaper := worker.New(dst, worker.WithDoModify(
				func(_ context.Context, t *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
					return []entroq.ModifyArg{t.Delete()}, nil
				}))
			return reaper.Run(gCtx, worker.Watching(pullworker.TombstoneQueue(pullInbox)))
		})

		return g.Wait()
	},
}

func init() {
	flags := pullCmd.Flags()
	flags.StringVar(&sourceEntroqAddr, "source-entroq", "", "Remote source EntroQ gRPC address to pull from (required).")
	flags.StringVar(&sourceCertFile, "source-cert", "", "TLS certificate for the source connection.")
	flags.StringVar(&sourceKeyFile, "source-key", "", "TLS private key for the source connection.")
	flags.StringVar(&sourceCAFile, "source-ca", "", "CA bundle for verifying the source peer.")
	flags.StringSliceVar(&pullSourceQueues, "source-queue", nil, "Source queue(s) to claim tasks from (required, repeatable).")
	flags.StringVar(&pullInbox, "inbox", "", "Local destination inbox queue (required).")
	flags.DurationVar(&pullTTL, "ttl", pullworker.DefaultTTL, "Tombstone retention window; must exceed worst-case recovery time.")
	flags.StringVar(&pullSourceName, "source-name", "", "Stable source identifier mixed into the transfer ID (defaults to --source-entroq).")
	flags.IntVar(&pullConcurrency, "concurrency", 1, "Number of concurrent pull workers.")

	pullCmd.MarkFlagRequired("source-entroq")
	pullCmd.MarkFlagRequired("source-queue")
	pullCmd.MarkFlagRequired("inbox")

	rootCmd.AddCommand(pullCmd)
}

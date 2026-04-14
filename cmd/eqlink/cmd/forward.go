package cmd

import (
	"fmt"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	destEntroqAddr string
	destCertFile   string
	destKeyFile    string
	destCAFile     string
	fwdSrcQueue    string
	fwdDstQueue    string
	fwdConcurrency int
)

var forwardCmd = &cobra.Command{
	Use:   "forward",
	Short: "Forward tasks from a local queue to a queue on a remote EntroQ instance.",
	Long: `Claims tasks from --queue on the local EntroQ instance (--entroq) and
re-inserts them into --dest-queue on the remote instance (--dest-entroq), then
deletes the source task.

Delivery is at-least-once: the remote insert and the local delete are separate
transactions. If the forwarder crashes between them the task will be
re-forwarded. Workers consuming from the destination queue should be designed
for idempotent processing.

This is the simplest cross-datacenter bridging primitive: run one forwarder per
outbound queue, pointed at the remote instance. No envelope wrapping, no
response routing -- task values are forwarded verbatim.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		src, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, eqgrpc.WithInsecure()))
		if err != nil {
			return fmt.Errorf("source entroq: %w", err)
		}
		defer src.Close()

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

		dst, err := entroq.New(ctx, eqgrpc.Opener(destEntroqAddr, destOpts...))
		if err != nil {
			return fmt.Errorf("dest entroq: %w", err)
		}
		defer dst.Close()

		fwd := async.NewForwarder(src, dst, fwdSrcQueue,
			async.WithForwarderDestQueue(fwdDstQueue),
			async.WithForwarderConcurrency(fwdConcurrency),
		)
		return fwd.Run(ctx)
	},
}

func init() {
	flags := forwardCmd.Flags()
	flags.StringVar(&fwdSrcQueue, "queue", "", "Source queue to claim tasks from (required).")
	flags.StringVar(&fwdDstQueue, "dest-queue", "", "Destination queue to insert into (defaults to --queue).")
	flags.StringVar(&destEntroqAddr, "dest-entroq", "", "Remote EntroQ gRPC address (required).")
	flags.StringVar(&destCertFile, "dest-cert", "", "TLS certificate for the remote EntroQ connection.")
	flags.StringVar(&destKeyFile, "dest-key", "", "TLS private key for the remote EntroQ connection.")
	flags.StringVar(&destCAFile, "dest-ca", "", "CA bundle for verifying the remote EntroQ peer.")
	flags.IntVar(&fwdConcurrency, "concurrency", 1, "Number of concurrent forwarding workers.")

	forwardCmd.MarkFlagRequired("queue")
	forwardCmd.MarkFlagRequired("dest-entroq")

	rootCmd.AddCommand(forwardCmd)
}

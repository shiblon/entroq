package cmd

import (
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/worker"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	myQueue        string
	senderAddr     string
	upstream       string
	concurrency    int
	requestTimeout time.Duration
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the full eqlink sidecar: sender, receiver, and GC.",
	Long: `Starts all three components:

  Sender:   listens on --addr, proxies outgoing HTTP calls into queues.
  Receiver: claims tasks from --queue, forwards them to --upstream.
  GC:       scans for stale response queues under --queue and cleans them up.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		eq, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, eqgrpc.WithInsecure()))
		if err != nil {
			return err
		}
		defer eq.Close()

		sender := async.NewSender(eq, senderAddr, myQueue,
			async.WithRequestTimeout(requestTimeout),
		)

		recvWorker := worker.New(eq,
			worker.WithDoModify(async.ReceiverHandler(upstream)),
		)

		g, ctx := errgroup.WithContext(ctx)

		g.Go(func() error { return sender.Run(ctx) })

		for range concurrency {
			g.Go(func() error { return recvWorker.Run(ctx, myQueue) })
		}

		g.Go(func() error {
			return async.RunGCLoop(ctx, eq, myQueue,
				async.WithGCInterval(gcInterval),
				async.WithGCGrace(gcGrace),
			)
		})

		return g.Wait()
	},
}

func init() {
	flags := runCmd.Flags()
	flags.StringVar(&myQueue, "queue", "", "This sidecar's queue name (required).")
	flags.StringVar(&senderAddr, "addr", ":8080", "Address for the sender to listen on.")
	flags.StringVar(&upstream, "upstream", "http://localhost:8000", "Upstream service address for the receiver.")
	flags.IntVar(&concurrency, "concurrency", 1, "Number of concurrent receiver goroutines.")
	flags.DurationVar(&requestTimeout, "request_timeout", 30*time.Second, "Sender request timeout.")
	flags.DurationVar(&gcInterval, "gc_interval", 10*time.Minute, "How often to run the GC scan.")
	flags.DurationVar(&gcGrace, "gc_grace", 15*time.Second, "Extra time after expiry before GC deletes a response queue.")
	runCmd.MarkFlagRequired("queue")

	rootCmd.AddCommand(runCmd)
}

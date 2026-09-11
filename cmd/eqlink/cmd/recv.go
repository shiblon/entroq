package cmd

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq/pkg/async"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var recvCmd = &cobra.Command{
	Use:   "recv",
	Short: "Run only the receiver: claims tasks from --queue and forwards them to --upstream.",
	Long: `Claims Envelope tasks from the specified queue, forwards each as an HTTP
request to the upstream service, and enqueues the Response task back to the
caller's response queue.

Use "eqlink run" to start the full sidecar (sender + receiver).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		mp, stopMetrics, err := setupMetrics(ctx)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		defer stopMetrics()

		g, gctx := errgroup.WithContext(ctx)
		eq, err := localEQ(gctx, g)
		if err != nil {
			return err
		}
		defer eq.Close()

		tlsCfg, err := loadTLSConfig(certFile, keyFile, caFile)
		if err != nil {
			return fmt.Errorf("load tls: %w", err)
		}

		rcvOpts := []async.ReceiverOption{
			async.WithReceiverMeterProvider(mp),
			async.WithReceiverConcurrency(concurrency),
			async.WithReceiverRequestTimeout(requestTimeout),
		}
		if tlsCfg != nil {
			rcvOpts = append(rcvOpts, async.WithReceiverTLSConfig(tlsCfg))
		}

		receiver := async.NewReceiver(eq, upstream, rcvOpts...)
		g.Go(func() error {
			if err := receiver.Run(gctx, myQueue+"/inbox"); err != nil {
				return fmt.Errorf("run receiver: %w", err)
			}
			return nil
		})
		return g.Wait()
	},
}

func init() {
	flags := recvCmd.Flags()
	flags.StringVar(&myQueue, "queue", "", "Service queue prefix (required). Receiver watches <prefix>/inbox.")
	flags.StringVar(&upstream, "upstream", "http://localhost:8000", "Upstream service address.")
	flags.IntVar(&concurrency, "concurrency", 1, "Number of concurrent receiver goroutines.")
	flags.DurationVar(&requestTimeout, "request_timeout", 3*time.Minute, "Maximum silence from the peer EQLink before ending a session; heartbeats are sent every third of this duration.")
	recvCmd.MarkFlagRequired("queue")

	rootCmd.AddCommand(recvCmd)
}

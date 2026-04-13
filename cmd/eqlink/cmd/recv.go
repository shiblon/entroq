package cmd

import (
	"fmt"
	"net/http"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var recvCmd = &cobra.Command{
	Use:   "recv",
	Short: "Run only the receiver: claims tasks from --queue and forwards them to --upstream.",
	Long: `Claims Envelope tasks from the specified queue, forwards each as an HTTP
request to the upstream service, and enqueues the Response task back to the
caller's response queue.

Use "eqlink run" to start the full sidecar (sender + receiver + GC).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		_, stopMetrics, err := setupMetrics(ctx)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		defer stopMetrics()

		eq, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, eqgrpc.WithInsecure()))
		if err != nil {
			return err
		}
		defer eq.Close()

		tlsCfg, err := loadTLSConfig(certFile, keyFile, caFile)
		if err != nil {
			return fmt.Errorf("load tls: %w", err)
		}

		var rcvOpts []async.ReceiverOption
		if tlsCfg != nil {
			rcvOpts = append(rcvOpts, async.WithReceiverHTTPClient(&http.Client{
				Transport: &http.Transport{
					TLSClientConfig:     tlsCfg,
					MaxIdleConnsPerHost: 32,
				},
			}))
		}

		recvWorker := worker.New(eq,
			worker.WithDoModify(async.ReceiverHandler(upstream, rcvOpts...)),
		)

		g, ctx := errgroup.WithContext(ctx)
		for range concurrency {
			g.Go(func() error {
				if err := recvWorker.Run(ctx, worker.Watching(myQueue)); err != nil {
					return fmt.Errorf("run worker: %w", err)
				}
				return nil
			})
		}
		return g.Wait()
	},
}

func init() {
	flags := recvCmd.Flags()
	flags.StringVar(&myQueue, "queue", "", "Queue to claim tasks from (required).")
	flags.StringVar(&upstream, "upstream", "http://localhost:8000", "Upstream service address.")
	flags.IntVar(&concurrency, "concurrency", 1, "Number of concurrent receiver goroutines.")
	recvCmd.MarkFlagRequired("queue")

	rootCmd.AddCommand(recvCmd)
}

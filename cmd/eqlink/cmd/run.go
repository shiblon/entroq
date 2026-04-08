package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	myQueue        string
	senderAddr     string
	upstream       string
	concurrency    int
	requestTimeout time.Duration
	drainTimeout   time.Duration
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the full eqlink sidecar: sender, receiver, and GC.",
	Long: `Starts all three components:

  Sender:   listens on --addr, proxies outgoing HTTP calls into queues.
  Receiver: claims tasks from --queue, forwards them to --upstream.
  GC:       scans for stale response queues under --queue and cleans them up.

Graceful shutdown on SIGINT/SIGTERM:
  1. Receiver workers stop claiming new tasks and finish any in-progress handler.
  2. Sender drains: waits for all in-flight requests to complete.
  3. GC shuts down.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigs)

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

		sender := async.NewSender(eq, senderAddr, myQueue,
			async.WithSenderRequestTimeout(requestTimeout),
		)

		g, _ := errgroup.WithContext(ctx)

		g.Go(func() error {
			return sender.Run(ctx)
		})

		rcvCtx, rcvCancel := context.WithCancel(ctx)
		defer rcvCancel()
		recvWorker := worker.New(eq,
			worker.WithDoModify(async.ReceiverHandler(upstream)),
		)
		for range concurrency {
			g.Go(func() error {
				return recvWorker.Run(rcvCtx, myQueue)
			})
		}

		gcCtx, gcCancel := context.WithCancel(ctx)
		defer gcCancel()
		g.Go(func() error {
			return async.RunGCLoop(gcCtx, eq, myQueue,
				async.WithGCInterval(gcInterval),
				async.WithGCGrace(gcGrace),
			)
		})

		// Signal handler: staged shutdown.
		g.Go(func() error {
			select {
			case <-ctx.Done():
				return nil
			case sig := <-sigs:
				log.Printf("received %v: stopping receivers", sig)
			}

			rcvCancel()
			log.Printf("receivers stopped")

			// Stage 2: drain and close the sender. srv.Shutdown waits for
			// active HTTP handlers to return, which includes the full
			// EntroQ round-trip for each in-flight request.
			log.Printf("draining sender (timeout: %v)...", drainTimeout)
			drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
			defer cancel()
			if err := sender.Close(drainCtx); err != nil {
				log.Printf("sender close: %v", err)
			}

			gcCancel()
			return nil
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
	flags.DurationVar(&drainTimeout, "drain_timeout", 35*time.Second, "How long to wait for in-flight requests to finish on shutdown.")
	flags.DurationVar(&gcInterval, "gc_interval", 10*time.Minute, "How often to run the GC scan.")
	flags.DurationVar(&gcGrace, "gc_grace", 15*time.Second, "Extra time after expiry before GC deletes a response queue.")
	runCmd.MarkFlagRequired("queue")

	rootCmd.AddCommand(runCmd)
}

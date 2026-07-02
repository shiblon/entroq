package cmd

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"path"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/gc"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

var (
	myQueue             string
	senderAddr          string
	upstream            string
	concurrency         int
	requestTimeout      time.Duration
	drainTimeout        time.Duration
	domainSuffix        string
	namespace           string
	auditLog            bool
	tokenReloadInterval time.Duration
	runGC               bool
	responseGrace       time.Duration
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

		grpcOpts := []eqgrpc.Option{eqgrpc.WithInsecure()}
		var creds *tokenFileCreds
		if authzTokenFile != "" {
			creds, err = newTokenFileCreds(authzTokenFile)
			if err != nil {
				return err
			}
			grpcOpts = append(grpcOpts, eqgrpc.WithDialOpts(
				grpc.WithPerRPCCredentials(creds),
			))
		}
		var eqOpts []entroq.Option
		if creds != nil {
			eqOpts = append(eqOpts, entroq.WithClaimantID(creds.claimant))
		}
		eq, err := entroq.New(ctx, eqgrpc.Opener(entroqAddr, grpcOpts...), eqOpts...)
		if err != nil {
			return err
		}
		defer eq.Close()

		tlsCfg, err := loadTLSConfig(certFile, keyFile, caFile)
		if err != nil {
			return fmt.Errorf("load tls: %w", err)
		}

		alog := newAuditLogger()
		sender := async.NewSender(eq, senderAddr,
			async.WithSenderRequestTimeout(requestTimeout),
			async.WithSenderResponseGrace(responseGrace),
			async.WithSenderTLSConfig(tlsCfg),
			async.WithSenderDomainSuffix(domainSuffix),
			async.WithSenderNamespace(namespace),
			async.WithSenderName(myQueue),
			async.WithSenderAuditLogger(alog),
		)

		g, gCtx := errgroup.WithContext(ctx)

		g.Go(func() error {
			return sender.Run(ctx)
		})

		var rcvOpts []async.ReceiverOption
		rcvOpts = append(rcvOpts,
			async.WithReceiverName(myQueue),
			async.WithReceiverAuditLogger(alog),
		)
		if tlsCfg != nil {
			rcvOpts = append(rcvOpts, async.WithReceiverHTTPClient(&http.Client{
				Transport: &http.Transport{
					TLSClientConfig:     tlsCfg,
					MaxIdleConnsPerHost: 32,
				},
			}))
		}

		rcvCtx, rcvCancel := context.WithCancel(gCtx)
		defer rcvCancel()
		recvWorker := worker.New(eq,
			worker.WithDoModify(async.ReceiverHandler(upstream, rcvOpts...)),
		)
		for range concurrency {
			g.Go(func() error {
				return recvWorker.Run(rcvCtx, worker.Watching(path.Join(myQueue, "inbox")))
			})
		}

		gcCtx, gcCancel := context.WithCancel(gCtx)
		defer gcCancel()
		if runGC {
			g.Go(func() error {
				return gc.RunLoop(gcCtx, eq,
					gc.WithMatch(entroq.MatchPrefix(myQueue)),
					gc.WithInterval(gcInterval),
				)
			})
		}

		// Token reload: reload the bearer token on rotation (SIGHUP or mtime).
		hupCtx, hupCancel := context.WithCancel(gCtx)
		if creds != nil {
			watchTokenReload(hupCtx, g, creds)
		}

		// Signal handler: staged shutdown. Also fires when any goroutine fails
		// (gCtx cancelled), ensuring the sender and GC are always cleaned up.
		g.Go(func() error {
			defer gcCancel()
			defer hupCancel()
			select {
			case <-gCtx.Done():
			case sig := <-sigs:
				log.Printf("received %v: stopping receivers", sig)
			}

			rcvCancel()
			log.Printf("receivers stopped")

			// Drain and close the sender. srv.Shutdown waits for active HTTP
			// handlers to return, including the full EntroQ round-trip.
			// Use a fresh context -- gCtx may already be cancelled here.
			log.Printf("draining sender (timeout: %v)...", drainTimeout)
			drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			if err := sender.Close(drainCtx); err != nil {
				log.Printf("sender close: %v", err)
			}

			return nil
		})

		return g.Wait()
	},
}

func init() {
	flags := runCmd.Flags()
	flags.StringVar(&myQueue, "queue", "", "This sidecar's service queue prefix (required). Receiver watches <prefix>/inbox.")
	flags.StringVar(&domainSuffix, "domain-suffix", ".localhost", "Domain suffix stripped from the Host header to derive the target service. E.g. .localhost or .eq.local.")
	flags.StringVar(&namespace, "namespace", "", "Default namespace prepended to single-label targets. E.g. payments makes bar.localhost route to payments/bar/inbox.")
	flags.StringVar(&senderAddr, "addr", ":8080", "Address for the sender to listen on.")
	flags.StringVar(&upstream, "upstream", "http://localhost:8000", "Upstream service address for the receiver.")
	flags.IntVar(&concurrency, "concurrency", 1, "Number of concurrent receiver goroutines.")
	flags.DurationVar(&requestTimeout, "request_timeout", 30*time.Second, "Sender request timeout.")
	flags.DurationVar(&drainTimeout, "drain_timeout", 35*time.Second, "How long to wait for in-flight requests to finish on shutdown.")
	flags.BoolVar(&runGC, "run-gc", true, "Run a local GC loop scoped to this sidecar's --queue prefix. On by default for now; will default off once the backends run GC server-side.")
	flags.DurationVar(&gcInterval, "gc_interval", 10*time.Minute, "How often to run the local GC scan (only when --run-gc is set).")
	flags.DurationVar(&responseGrace, "response_grace", 15*time.Second, "Margin added past --request_timeout when stamping the response queue's collectable-at time, so GC does not delete a response still being awaited. Size to worst-case sender/GC clock skew.")
	flags.BoolVar(&auditLog, "audit-log", false, "Emit structured JSON audit events to stderr for every request mediated (request_enqueued, request_handled, response_received).")
	flags.DurationVar(&tokenReloadInterval, "token-reload-interval", 5*time.Minute, "How often to stat the --authz-token-file and reload it if changed. Handles k8s projected token rotation.")
	runCmd.MarkFlagRequired("queue")

	rootCmd.AddCommand(runCmd)
}

// newAuditLogger returns a JSON slog.Logger writing to stderr when --audit-log
// is set, or nil (disabled) otherwise.
func newAuditLogger() *slog.Logger {
	if !auditLog {
		return nil
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

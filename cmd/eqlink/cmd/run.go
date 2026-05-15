package cmd

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"path"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
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

		hupsigs := make(chan os.Signal, 1)
		signal.Notify(hupsigs, syscall.SIGHUP)
		defer signal.Stop(hupsigs)

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
		g.Go(func() error {
			return async.RunGCLoop(gcCtx, eq, myQueue,
				async.WithGCInterval(gcInterval),
				async.WithGCGrace(gcGrace),
			)
		})

		// Token reload: periodically stat the token file and reload on mtime
		// change. k8s projected tokens are rotated by the kubelet (typically
		// at 80% of the 1h TTL) by updating the file; SIGHUP is also accepted
		// for manual/scripted reloads.
		hupCtx, hupCancel := context.WithCancel(gCtx)
		if creds != nil {
			g.Go(func() error {
				info, _ := os.Stat(authzTokenFile)
				var lastMtime time.Time
				if info != nil {
					lastMtime = info.ModTime()
				}
				ticker := time.NewTicker(tokenReloadInterval)
				defer ticker.Stop()
				for {
					select {
					case <-hupCtx.Done():
						return nil
					case <-hupsigs:
						if err := creds.reload(); err != nil {
							log.Printf("token reload (SIGHUP): %v", err)
						} else {
							log.Printf("token reloaded (SIGHUP) from %s", authzTokenFile)
						}
					case <-ticker.C:
						fi, err := os.Stat(authzTokenFile)
						if err != nil {
							log.Printf("token stat %s: %v", authzTokenFile, err)
							continue
						}
						if fi.ModTime().After(lastMtime) {
							lastMtime = fi.ModTime()
							if err := creds.reload(); err != nil {
								log.Printf("token reload (stat): %v", err)
							} else {
								log.Printf("token reloaded (stat) from %s", authzTokenFile)
							}
						}
					}
				}
			})
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
	flags.StringVar(&myQueue, "queue", "", "This sidecar's service queue prefix (required). Receiver watches <prefix>/inbox; GC scans <prefix>/.")
	flags.StringVar(&domainSuffix, "domain-suffix", ".localhost", "Domain suffix stripped from the Host header to derive the target service. E.g. .localhost or .eq.local.")
	flags.StringVar(&namespace, "namespace", "", "Default namespace prepended to single-label targets. E.g. payments makes bar.localhost route to payments/bar/inbox.")
	flags.StringVar(&senderAddr, "addr", ":8080", "Address for the sender to listen on.")
	flags.StringVar(&upstream, "upstream", "http://localhost:8000", "Upstream service address for the receiver.")
	flags.IntVar(&concurrency, "concurrency", 1, "Number of concurrent receiver goroutines.")
	flags.DurationVar(&requestTimeout, "request_timeout", 30*time.Second, "Sender request timeout.")
	flags.DurationVar(&drainTimeout, "drain_timeout", 35*time.Second, "How long to wait for in-flight requests to finish on shutdown.")
	flags.DurationVar(&gcInterval, "gc_interval", 10*time.Minute, "How often to run the GC scan.")
	flags.DurationVar(&gcGrace, "gc_grace", 15*time.Second, "Extra time after expiry before GC deletes a response queue.")
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

// tokenFileCreds implements grpc.PerRPCCredentials with an in-memory token
// that is loaded once at startup and reloaded on SIGHUP.
type tokenFileCreds struct {
	path     string
	claimant string // sub#nonce, computed once at startup
	token    atomic.Pointer[string]
}

// newTokenFileCreds loads the token immediately; returns an error if the file
// cannot be read.
func newTokenFileCreds(path string) (*tokenFileCreds, error) {
	c := &tokenFileCreds{path: path}
	if err := c.reload(); err != nil {
		return nil, err
	}
	c.claimant = entroq.MustClaimantFromSub(*c.token.Load())
	return c, nil
}

func (c *tokenFileCreds) reload() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("bearer token file %q: %w", c.path, err)
	}
	tok := strings.TrimSpace(string(data))
	c.token.Store(&tok)
	return nil
}

func (c *tokenFileCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + *c.token.Load()}, nil
}

func (*tokenFileCreds) RequireTransportSecurity() bool {
	return false
}


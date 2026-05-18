package cmd

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/spf13/cobra"
)

var sendCmd = &cobra.Command{
	Use:   "send",
	Short: "Run only the sender: listens on --addr and proxies HTTP calls into queues.",
	Long: `Starts the HTTP proxy server. Incoming requests are translated into Envelope
tasks on the target queue and the sender blocks waiting for a Response task on
an ephemeral per-request response queue.

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

		sender := async.NewSender(eq, senderAddr,
			async.WithSenderRequestTimeout(requestTimeout),
			async.WithSenderTLSConfig(tlsCfg),
			async.WithSenderDomainSuffix(domainSuffix),
			async.WithSenderNamespace(namespace),
		)
		return sender.Run(ctx)
	},
}

func init() {
	flags := sendCmd.Flags()
	flags.StringVar(&senderAddr, "addr", ":8080", "Address to listen on.")
	flags.DurationVar(&requestTimeout, "request_timeout", 30*time.Second, "How long the sender waits for a response task before returning 504.")
	flags.StringVar(&domainSuffix, "domain-suffix", ".localhost", "Domain suffix stripped from Host header to derive the target service.")
	flags.StringVar(&namespace, "namespace", "", "Default namespace prepended to single-label targets.")
	rootCmd.AddCommand(sendCmd)
}

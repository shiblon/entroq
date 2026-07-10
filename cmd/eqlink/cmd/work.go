package cmd

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/workgateway"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	workAddr  string
	workLease time.Duration
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Run a worker gateway: drive a language-agnostic worker over a JSON protocol.",
	Long: `Runs the EntroQ worker loop (claim, renew, commit) on behalf of a worker written
in any language, which speaks a small newline-delimited JSON protocol and never
touches EntroQ, gRPC, or the queue API.

The worker's first message is a register declaring the queues it serves and which
optional phases it implements:

    {"type":"register","queues":["/my/inbox"],"maxAttempts":5,"takeDocs":false,"cleanup":false}

Then, per claimed task, the gateway sends the registered phases and reads a reply:

    gateway -> {"type":"takeDocs","task":{...}}          # only if takeDocs registered
    client  -> {"type":"docs","claims":[{"namespace":..,"key":..}]}
    gateway -> {"type":"doWork","task":{...},"docs":[...]}
    client  -> {"type":"result","outcome":"ok","modification":{...}}
    gateway -> {"type":"cleanup"}                        # only if cleanup registered
    client  -> {"type":"done"}

"ok" commits the (possibly empty) modification; "ok" alone does not delete the
task. "retry"/"move"/"fatal" map to the worker's structured errors. Diagnostics
go to stderr; stdout carries only the protocol.

By default one worker runs over this process's stdin/stdout. With --addr, the
gateway serves WebSocket instead and each connecting worker is one slot.
Concurrency is more connections; dropping a connection stops renewal and the
in-flight task is reclaimed on lease expiry.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		g, gctx := errgroup.WithContext(ctx)

		eq, err := localEQ(gctx, g)
		if err != nil {
			return err
		}
		defer eq.Close()

		// WebSocket serve mode: many workers connect, each registering its own queues.
		if workAddr != "" {
			return workgateway.Serve(gctx, workAddr, eq, workLease)
		}

		// stdio mode: one worker over this process's stdin/stdout.
		bridge := workgateway.NewBridge(workgateway.NewPipeConn(os.Stdin, os.Stdout))
		return bridge.Run(gctx, eq, workLease)
	},
}

func init() {
	flags := workCmd.Flags()
	flags.StringVar(&workAddr, "addr", "", "If set, serve the gateway over WebSocket on this address (workers connect to /work). Otherwise run one worker over stdio.")
	flags.DurationVar(&workLease, "lease", entroq.DefaultClaimDuration, "Claim lease and renewal interval (gateway-owned; not client-chosen).")

	rootCmd.AddCommand(workCmd)
}

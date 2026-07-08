package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/shiblon/entroq/pkg/workgateway"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	workAddr        string
	workQueues      []string
	workLease       time.Duration
	workMaxAttempts int32
)

var workCmd = &cobra.Command{
	Use:   "work --queue Q [--queue Q...]",
	Short: "Run a worker gateway: drive a language-agnostic worker over stdio.",
	Long: `Claims tasks from the given queues on --entroq and hands each to a worker over
stdin/stdout using a small newline-delimited JSON protocol, so the worker can be
written in any language without touching EntroQ, gRPC, or the queue API.

Skeleton: implements only the "work" phase. For each claimed task it writes
{"type":"work","task":{...}} to stdout and reads one
{"type":"result","outcome":"ok|retry|move|fatal", ...} from stdin. "ok" consumes
the task; "retry"/"move"/"fatal" map to the worker's structured errors (with
optional "after"/"orMove"/"to"). Diagnostics go to stderr; stdout carries only
the protocol.

Concurrency is more processes; killing this one (or closing its stdin) stops
renewal and the in-flight task is reclaimed.`,
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

		// WebSocket serve mode: workers connect and declare queues in the URL.
		if workAddr != "" {
			return workgateway.Serve(gctx, workAddr, eq, workLease)
		}

		// stdio mode: one worker over this process's stdin/stdout.
		if len(workQueues) == 0 {
			return fmt.Errorf("--queue is required in stdio mode (or set --addr to serve WebSocket)")
		}
		bridge := workgateway.NewBridge(workgateway.NewPipeConn(os.Stdin, os.Stdout))
		w := worker.New(eq, worker.WithDoModify[json.RawMessage](bridge.DoWork))
		return w.Run(gctx,
			worker.Watching(workQueues...),
			worker.WithLease(workLease),
			worker.WithMaxAttempts(workMaxAttempts),
		)
	},
}

func init() {
	flags := workCmd.Flags()
	flags.StringVar(&workAddr, "addr", "", "If set, serve the gateway over WebSocket on this address; workers connect with /work?queue=... . Otherwise run one worker over stdio.")
	flags.StringArrayVar(&workQueues, "queue", nil, "Queue to listen on in stdio mode (repeatable). In --addr mode, queues come from the connection URL.")
	flags.DurationVar(&workLease, "lease", entroq.DefaultClaimDuration, "Claim lease and renewal interval (gateway-owned).")
	flags.Int32Var(&workMaxAttempts, "max-attempts", 0, "Max attempts before a retry is quarantined in stdio mode; 0 means unlimited.")

	rootCmd.AddCommand(workCmd)
}

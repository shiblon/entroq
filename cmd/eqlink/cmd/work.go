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
	workAddr        string
	workLease       time.Duration
	workQueues      []string
	workMaxAttempts int32
	workTakeDocs    bool
	workWork        bool
	workCleanup     bool
)

var workCmd = &cobra.Command{
	Use:   "work --queue Q [--queue Q...] --work",
	Short: "Run a worker gateway: drive a language-agnostic worker over a JSON protocol.",
	Long: `Runs the EntroQ worker loop (claim, renew, commit) on behalf of a worker written
in any language, which speaks a small newline-delimited JSON protocol and never
touches EntroQ, gRPC, or the queue API.

Registration is out-of-band and set at connection time. In stdio mode (the
default) the client typically spawns this process and declares its registration
with flags: the queues it serves, its max-attempts, and which phases it
implements (--take-docs, --work, --cleanup). A work handler is required.

Then, per claimed task, the gateway sends only the registered phases and reads a
reply:

    gateway -> {"type":"takeDocs","task":{...}}          # only if --take-docs
    client  -> {"type":"docs","claims":[{"namespace":..,"key":..}]}
    gateway -> {"type":"doWork","task":{...},"docs":[...]}
    client  -> {"type":"result","outcome":"ok","modification":{...}}
    gateway -> {"type":"cleanup"}                        # only if --cleanup
    client  -> {"type":"done"}

"ok" commits the (possibly empty) modification; "ok" alone does not delete the
task. "retry"/"move"/"fatal" map to the worker's structured errors. Diagnostics
go to stderr; stdout carries only the protocol.

With --addr the gateway serves WebSocket instead, and each connecting worker
declares the same registration via URL query params (?queue=..&work=1&...).
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

		// WebSocket serve mode: many workers connect, each declaring its own
		// registration via URL query params.
		if workAddr != "" {
			return workgateway.Serve(gctx, workAddr, eq, workLease)
		}

		// stdio mode: one worker over this process's stdin/stdout, registered by
		// this command's flags.
		cfg := workgateway.Config{
			Queues:      workQueues,
			MaxAttempts: workMaxAttempts,
			TakeDocs:    workTakeDocs,
			Work:        workWork,
			Cleanup:     workCleanup,
		}
		bridge := workgateway.NewBridge(workgateway.NewPipeConn(os.Stdin, os.Stdout))
		return bridge.Run(gctx, eq, cfg, workLease)
	},
}

func init() {
	flags := workCmd.Flags()
	flags.StringVar(&workAddr, "addr", "", "If set, serve the gateway over WebSocket on this address (workers connect to /work and register via URL params). Otherwise run one worker over stdio.")
	flags.DurationVar(&workLease, "lease", entroq.DefaultClaimDuration, "Claim lease and renewal interval (gateway-owned; not client-chosen).")
	flags.StringArrayVar(&workQueues, "queue", nil, "A queue this worker serves (repeatable). Required in stdio mode.")
	flags.Int32Var(&workMaxAttempts, "max-attempts", 0, "Max attempts before a retry is quarantined; 0 means unlimited.")
	flags.BoolVar(&workTakeDocs, "take-docs", false, "The worker implements the takeDocs phase.")
	flags.BoolVar(&workWork, "work", false, "The worker implements the work phase (required).")
	flags.BoolVar(&workCleanup, "cleanup", false, "The worker implements the cleanup phase.")

	rootCmd.AddCommand(workCmd)
}

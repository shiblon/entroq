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
	workAddr          string
	workLease         time.Duration
	workQueues        []string
	workMaxAttempts   int32
	workTakeDocs      bool
	workWork          bool
	workSuccess       bool
	workDependency    bool
	workEntroQTimeout time.Duration
)

var workCmd = &cobra.Command{
	Use:   "work --queue Q [--queue Q...] --work",
	Short: "Run a worker gateway: drive a language-agnostic worker over a JSON protocol.",
	Long: `Runs the EntroQ worker loop (claim, renew, commit) on behalf of a worker written
in any language, which speaks a small newline-delimited JSON protocol and never
touches EntroQ, gRPC, or the queue API.

Every domain object on the wire (task, docs, modification, dependency list) is
the canonical protojson of the corresponding message in api/entroq.proto, so a
worker generates those types from the same proto the rest of EntroQ uses and
hand-models nothing. Only the thin envelope around them (the "type" tag and the
phase framing) is gateway-specific.

Registration is out-of-band and set at connection time. In stdio mode (the
default) the client typically spawns this process and declares its registration
with flags: the queues it serves, its max-attempts, and which phases it
implements (--take-docs, --work, --success, --dependency). A work handler is
required.

Then, per claimed task, the gateway sends only the registered phases and reads a
reply. Exactly one post-commit phase (success or dependency) fires, and only if
registered:

    gateway -> {"type":"takeDocs","task":{...}}          # only if --take-docs
    client  -> {"type":"docs","claims":[{"namespace":..,"key":..}]}
    gateway -> {"type":"doWork","task":{...},"docs":[...]}
    client  -> {"type":"result","outcome":"ok","ack":true,"modification":{...}}
    gateway -> {"type":"success"}                        # commit ok; only if --success
    client  -> {"type":"done","outcome":"ok"}
    gateway -> {"type":"dependency","deps":[...]}        # commit lost a dependency; only if --dependency
    client  -> {"type":"done","outcome":"ok"}

The modification is a protojson ModifyRequest; leave its claimant_id empty, as
the gateway owns the claim and attributes the commit itself. "ok" commits the
(possibly empty) modification; "ok" alone does not delete the task. Set
"ack":true as the shorthand for "I consumed this task" and the gateway also
deletes it (unless the modification already disposes of it, in which case the
modification wins). "retry"/"move"/"fatal" map to the worker's structured
errors. If the commit loses a dependency race, the gateway reports the failed
dependencies (a protojson ModifyDep list) and the done outcome picks the task's
fate the same way. Diagnostics go to stderr; stdout carries only the protocol,
so a spawning client should let this process inherit its stderr to see them.

A worker error that does not itself drop the connection (a transient EntroQ
outage being retried, or a caller/gateway fault about to stop the worker) is
reported to the client out of band as {"type":"error","class":...,"message":...}
in place of the next phase message; it is one-way, the client decides what to do.
The gateway rides out an unreachable EntroQ backend (one being restarted or
relocated) for --entroq-timeout, reconnecting transparently, before giving up.
On stop it exits with a class code a supervisor can key on: 0 clean, 75
transient (retryable), 78 caller fault, 70 gateway fault.

With --addr the gateway serves WebSocket instead, and each connecting worker
declares the same registration via URL query params (?queue=..&work=1&...).
Concurrency is more connections; a dropped connection is a clean stop and the
in-flight task is reclaimed on lease expiry. The same classes surface as
WebSocket close codes (1013 transient, 1008 caller, 1011 gateway).`,
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
			Success:     workSuccess,
			Dependency:  workDependency,
		}
		bridge := workgateway.NewBridge(workgateway.NewPipeConn(os.Stdin, os.Stdout),
			workgateway.WithConfig(cfg), workgateway.WithLease(workLease),
			workgateway.WithEntroQTimeout(workEntroQTimeout))
		return bridge.Run(gctx, eq)
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
	flags.BoolVar(&workSuccess, "success", false, "The worker implements the success phase (post-commit).")
	flags.BoolVar(&workDependency, "dependency", false, "The worker implements the dependency phase (commit lost a dependency).")
	flags.DurationVar(&workEntroQTimeout, "entroq-timeout", 60*time.Second, "How long to ride out an unreachable EntroQ backend (restart or relocation) before exiting; 0 disables the ride-out.")

	rootCmd.AddCommand(workCmd)
}

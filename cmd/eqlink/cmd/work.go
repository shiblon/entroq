package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/shiblon/entroq/pkg/workgateway"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var (
	workAddr           string
	workLease          time.Duration
	workMaxWorkTimeout time.Duration
	workMaxFrameMB     int
)

var workCmd = &cobra.Command{
	Use:   "work [--addr ADDR]",
	Short: "Run a work gateway: host a worker written in any language over a JSON protocol.",
	Long: `Runs the EntroQ worker loop (claim, renew, commit) on behalf of a worker written
in any language, which speaks a small JSON protocol and never touches EntroQ,
gRPC, or the queue API.

The client drives. It speaks first, with a config naming the queues it serves
and the phases it wants, and the gateway answers with a check naming its session
and claimant. The client says it is ready, and from then on the gateway hands it
one task at a time and waits for its answer:

    client  -> {"type":"config","config":{"queues":["in"],"protocol_versions":[1]}}
    gateway -> {"type":"check","session":"...","claimant":"...","expect":"ready"}
    client  -> {"type":"ready","session":"..."}
    gateway -> {"type":"work","task":{...},"expect":"modify"}
    client  -> {"type":"modify","session":"...","modification":{...}}
    gateway -> {"type":"work",...}                       # the next task

Every domain object on the wire (task, docs, modification, dependency list) is
the canonical protojson of the corresponding message in api/entroq.proto. The
modification is a ModifyRequest: it says everything about the task's fate, so a
task the client consumed is one it deletes. Leave its claimant_id empty; the
gateway owns the claim. An outcome of "retry", "move", "fatal" or "error"
replaces the commit with that disposition. Setting "quit" on a modify commits it
and then ends the session, answered with {"type":"quit"}.

With no --addr, one session runs over stdio: a client spawns this process and
writes to its stdin, and reads its stdout, one JSON value per line. Diagnostics
go to stderr. The first SIGINT or SIGTERM waits up to --lease for the client to
quit; a second stops at once, leaving any task in hand to its lease.

With --addr, sessions run over HTTP: the client POSTs each message and gets the
gateway's answer as the response, and a session spans as many connections as it
likes. The first SIGINT or SIGTERM ends every session and stops the server,
leaving any task in hand to its lease.

On stop it exits with a class code a supervisor can key on: 0 clean, 75
transient (retryable), 78 caller fault, 70 gateway fault.

See examples/workgateway for a client.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if workMaxFrameMB < 0 {
			return fmt.Errorf("--max-frame-mb must not be negative")
		}
		opts := []workgateway.Option{
			workgateway.WithLease(workLease),
			workgateway.WithMaxWorkTimeout(workMaxWorkTimeout),
			workgateway.WithMaxFrameBytes(workMaxFrameMB << 20),
		}
		if workAddr != "" {
			return serveWorkHTTP(workAddr, opts)
		}
		return serveWorkStdio(opts)
	},
}

// serveWorkStdio runs one session over stdin and stdout, for a client that
// spawned this process.
func serveWorkStdio(opts []workgateway.Option) error {
	// A quit is the client's to send, so the drain is waiting for one: a
	// client in the same process group got the same signal and is finishing.
	served := make(chan struct{})
	ctx, stop := onSignal(context.Background(), workLease, func(sctx context.Context) error {
		select {
		case <-served:
			return nil
		case <-sctx.Done():
			return fmt.Errorf("session still open: %w", sctx.Err())
		}
	})
	defer stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	eq, err := localEQ(gctx, g)
	if err != nil {
		return err
	}
	defer eq.Close()

	g.Go(func() error {
		defer close(served)
		// The session is the whole run, so its end releases whatever else
		// the group is holding open.
		defer cancel()
		conn := workgateway.NewStreamConn(workgateway.Joining(os.Stdin, os.Stdout), opts...)
		return workgateway.Serve(gctx, eq, conn, opts...)
	})
	return g.Wait()
}

// serveWorkHTTP serves sessions over HTTP on addr until a signal stops it.
func serveWorkHTTP(addr string, opts []workgateway.Option) error {
	// Sessions live on a context of their own, because a waiting request is
	// a session waiting on a claim: the server cannot shut down around them
	// until they have ended.
	sessCtx, endSessions := context.WithCancel(context.Background())
	defer endSessions()

	srv := &http.Server{Addr: addr}
	ctx, stop := onSignal(context.Background(), workLease, func(sctx context.Context) error {
		endSessions()
		return srv.Shutdown(sctx)
	})
	defer stop()

	g, gctx := errgroup.WithContext(ctx)
	eq, err := localEQ(gctx, g)
	if err != nil {
		return err
	}
	defer eq.Close()

	h := workgateway.NewHandler(sessCtx, eq, opts...)
	srv.Handler = h

	g.Go(func() error {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve %s: %w", addr, err)
		}
		return nil
	})
	// A second signal, or a failure anywhere in the group, skips the drain.
	g.Go(func() error {
		<-gctx.Done()
		endSessions()
		return srv.Close()
	})
	return g.Wait()
}

func init() {
	flags := workCmd.Flags()
	flags.StringVar(&workAddr, "addr", "", "If set, serve sessions over HTTP on this address. Otherwise run one session over stdio.")
	flags.DurationVar(&workLease, "lease", entroq.DefaultClaimDuration, "Claim lease and renewal interval, and how long the first signal waits for a stdio client to quit.")
	flags.DurationVar(&workMaxWorkTimeout, "max-work-timeout", worker.DefaultWorkTimeout, "Longest a client may hold one task, and what one that asks for nothing gets; 0 lets a client ask for any bound, including none.")
	flags.IntVar(&workMaxFrameMB, "max-frame-mb", workgateway.DefaultMaxFrameBytes>>20, "Largest message in MB, in either direction; 0 is no bound.")

	rootCmd.AddCommand(workCmd)
}

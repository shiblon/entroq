package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
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
	workParentPID      int
	workDetached       bool
)

// startPPID is this process's parent, read as early as Go can read it: the
// parent an --addr gateway watches when --parent-pid does not name one.
var startPPID = os.Getppid()

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
likes. A client that stops waiting for an answer has left, and its session
ends. Once listening, the gateway writes one line to stdout and nothing else
after it:

    {"addr":"127.0.0.1:43117"}

so a parent can pass port 0 and read where to connect. The gateway stops when
its parent process exits, checked once a second against --parent-pid, or the
parent it started with; --detached turns that off, for a gateway run as a
service. The first SIGINT or SIGTERM ends every session and stops the server,
leaving any task in hand to its lease.

It exits with a class code a supervisor can key on: 0 clean, 75 transient
(retryable, including EntroQ unreachable at startup), 78 caller fault
(including bad flags or credentials), 70 gateway fault.

See examples/workgateway for a client.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if workMaxFrameMB < 0 {
			return badFlag("--max-frame-mb must not be negative")
		}
		if workParentPID < 0 {
			return badFlag("--parent-pid must not be negative")
		}
		if workAddr == "" && (workParentPID != 0 || workDetached) {
			return badFlag("--parent-pid and --detached apply only with --addr")
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
	// The session drains: it finishes the task in hand and answers with a quit.
	// Before the server exists there is nothing to drain.
	var gw *workgateway.Server
	started := make(chan struct{})
	ctx, stop := onSignal(context.Background(), workLease, func(sctx context.Context) error {
		select {
		case <-started:
		default:
			return nil
		}
		if err := gw.Shutdown(sctx); err != nil {
			return fmt.Errorf("session still open: %w", err)
		}
		return nil
	})
	defer stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	eq, err := workEQ(gctx, g)
	if errors.Is(err, errStopped) {
		// Explicit stop is not an error, just exit cleanly.
		return nil
	}
	if err != nil {
		return err
	}
	defer eq.Close()

	gw = workgateway.NewServer(gctx, eq, opts...)
	close(started)
	g.Go(func() error {
		// The session is the whole run, so its end releases whatever else
		// the group is holding open.
		defer cancel()
		conn := workgateway.NewStreamConn(workgateway.Joining(os.Stdin, os.Stdout), opts...)
		return gw.ServeConn(gctx, conn)
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

	// Each session drains as a stdio one does, and the server shuts down once
	// they all have. Before the gateway exists there is nothing to drain.
	srv := &http.Server{}
	var gw *workgateway.Server
	started := make(chan struct{})
	ctx, stop := onSignal(context.Background(), workLease, func(sctx context.Context) error {
		select {
		case <-started:
		default:
			return nil
		}
		if err := gw.Shutdown(sctx); err != nil {
			return fmt.Errorf("sessions still open: %w", err)
		}
		return srv.Shutdown(sctx)
	})
	defer stop()

	g, gctx := errgroup.WithContext(ctx)
	eq, err := workEQ(gctx, g)
	if errors.Is(err, errStopped) {
		// Explicit stop is not an error, just exit cleanly.
		return nil
	}
	if err != nil {
		return err
	}
	defer eq.Close()

	gw = workgateway.NewServer(sessCtx, eq, opts...)
	srv.Handler = gw
	close(started)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return workgateway.NewExitError(workgateway.ExitCaller, fmt.Errorf("listen: %w", err))
	}
	// The only thing this process writes to stdout, and the first sign a
	// parent gets that it can connect.
	announce := struct {
		Addr string `json:"addr"`
	}{ln.Addr().String()}
	if err := json.NewEncoder(os.Stdout).Encode(announce); err != nil {
		if cerr := ln.Close(); cerr != nil {
			log.Printf("work gateway: close listener: %v", cerr)
		}
		return fmt.Errorf("announce address: %w", err)
	}
	log.Printf("work gateway: serving HTTP on %s", announce.Addr)

	g.Go(func() error {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve %s: %w", announce.Addr, err)
		}
		return nil
	})
	if !workDetached {
		parent := startPPID
		if workParentPID != 0 {
			parent = workParentPID
		}
		g.Go(func() error {
			watchParent(gctx, parent, time.Second, stop)
			return nil
		})
	}
	// A second signal, or a failure anywhere in the group, skips the drain.
	g.Go(func() error {
		<-gctx.Done()
		endSessions()
		return srv.Close()
	})
	return g.Wait()
}

// watchParent calls stop once parent is no longer this process's parent,
// checking at once and then every interval, until ctx ends. A parent that exits
// leaves this process to init or a subreaper, which changes what Getppid
// reports.
func watchParent(ctx context.Context, parent int, every time.Duration, stop func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if ppid := os.Getppid(); ppid != parent {
			log.Printf("work gateway: parent %d has exited (parent is now %d); stopping", parent, ppid)
			stop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// errStopped is a start-up that a stop cut short, which is a clean stop.
var errStopped = errors.New("stopped during start-up")

// workEQ opens EntroQ for a gateway, classifying a failure the way a session's
// exit is classified: an EntroQ that never answered is transient, and anything
// else is configuration a retry will not fix. A stop during the dial is
// errStopped.
func workEQ(ctx context.Context, g *errgroup.Group) (*entroq.EntroQ, error) {
	eq, err := localEQ(ctx, g)
	switch {
	case err == nil:
		return eq, nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%w: %w", errStopped, err)
	case errors.Is(err, context.DeadlineExceeded) || entroq.IsUnavailable(err):
		return nil, workgateway.NewExitError(workgateway.ExitTransient, err)
	default:
		return nil, workgateway.NewExitError(workgateway.ExitCaller, err)
	}
}

// badFlag refuses a flag value as a caller fault, which a retry will not fix.
func badFlag(msg string) error {
	return workgateway.NewExitError(workgateway.ExitCaller, errors.New(msg))
}

func init() {
	flags := workCmd.Flags()
	flags.StringVar(&workAddr, "addr", "", "If set, serve sessions over HTTP on this address. Otherwise run one session over stdio.")
	flags.DurationVar(&workLease, "lease", entroq.DefaultClaimDuration, "Claim lease and renewal interval, and how long the first signal waits for a stdio client to quit.")
	flags.DurationVar(&workMaxWorkTimeout, "max-work-timeout", worker.DefaultWorkTimeout, "Longest a client may hold one task, and what one that asks for nothing gets; 0 lets a client ask for any bound, including none.")
	flags.IntVar(&workMaxFrameMB, "max-frame-mb", workgateway.DefaultMaxFrameBytes>>20, "Largest message in MB, in either direction; 0 is no bound.")
	flags.IntVar(&workParentPID, "parent-pid", 0, "With --addr, the parent process to stop with; a parent passes its own pid. 0 watches the parent this process started with.")
	flags.BoolVar(&workDetached, "detached", false, "With --addr, keep serving after the parent process exits, for a gateway run as a service.")

	rootCmd.AddCommand(workCmd)
}

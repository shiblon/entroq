package procworker

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"testing"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/mem"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

func waitEmpty(ctx context.Context, eq *entroq.EntroQ, q string) error {
	for {
		empty, err := eq.QueuesEmpty(ctx, entroq.MatchExact(q))
		if err != nil {
			return errors.Wrap(err, "queue empty")
		}
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Wrap(err, "queue empty")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func mustCleanPath(p string) string {
	explicitPath, err := filepath.EvalSymlinks(p)
	if err != nil {
		log.Fatalf("Can't eval symlinks on path %q", p)
	}
	return filepath.Clean(explicitPath)
}

func TestRun(t *testing.T) {
	ctx := context.Background()

	eq, err := entroq.New(ctx, mem.Opener())
	if err != nil {
		t.Fatalf("Can't open eq: %v", err)
	}
	defer eq.Close()

	const inbox = "/subproctest/inbox"
	const implicitOutbox = inbox + "/done"

	cases := []struct {
		in     *SubprocessInput
		expect *SubprocessOutput
	}{
		{
			in: &SubprocessInput{
				Cmd: []string{"/bin/bash", "-c", `echo "output here, var=${VAR}"; echo 1>&2 'error here'`},
				Env: []string{"VAR=my value"},
				// Implicit outbox.
			},
			expect: &SubprocessOutput{
				Cmd:    []string{"/bin/bash", "-c", `echo "output here, var=${VAR}"; echo 1>&2 'error here'`},
				Env:    []string{"VAR=my value"},
				Stdout: "output here, var=my value\n",
				Stderr: "error here\n",
			},
		},
		{
			in: &SubprocessInput{
				Cmd:    []string{"pwd"},
				Dir:    "/tmp",
				Outbox: "/special/outbox",
			},
			expect: &SubprocessOutput{
				Cmd:    []string{"pwd"},
				Dir:    "/tmp",
				Stdout: mustCleanPath("/tmp") + "\n",
			},
		},
	}

	// Start the worker.
	ctx, cancel := context.WithCancel(ctx)
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		w, err := eq.NewWorker(entroq.WorkOn(inbox))
		if err != nil {
			return err
		}

		return w.Run(ctx, Run)
	})

	// Insert tasks one at a time, check that we get the expected output in the expected place.
	for _, test := range cases {
		if _, _, err := eq.Modify(ctx, entroq.InsertingInto(inbox, entroq.WithValue(test.in.JSON()))); err != nil {
			t.Fatalf("Error inserting task into %q: %v", inbox, err)
		}
		if err := waitEmpty(ctx, eq, inbox); err != nil {
			t.Fatalf("Error waiting for queue %q to empty: %v", inbox, err)
		}
		// Processed the task. Check that it showed up in the right place.
		outbox := test.in.Outbox
		if outbox == "" {
			outbox = implicitOutbox
		}

		task, err := eq.Claim(ctx, entroq.From(outbox), entroq.ClaimFor(10*time.Second))
		if err != nil {
			t.Fatalf("Claim from outbox %q: %v", outbox, err)
		}

		output := new(SubprocessOutput)
		if err := json.Unmarshal(task.Value, output); err != nil {
			t.Fatalf("Unmarshal output task: %v", err)
		}

		if diff := cmp.Diff(test.expect, output); diff != "" {
			t.Errorf("Unexpected output (-want +got):\n%v", diff)
		}
	}

	cancel()
	if err := g.Wait(); err != nil && !entroq.IsCanceled(err) {
		t.Fatalf("Unexpected error: %v", err)
	}
}

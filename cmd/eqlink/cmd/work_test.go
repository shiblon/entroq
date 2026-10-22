package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/workgateway"
	"golang.org/x/sync/errgroup"
)

// TestWatchParent_StopsAtOnce: a parent that is already not this process's
// parent stops it on the first check, without waiting an interval. That first
// check is what makes --parent-pid catch a parent that exited before startup.
func TestWatchParent_StopsAtOnce(t *testing.T) {
	stopped := make(chan struct{})
	go watchParent(context.Background(), os.Getppid()+1, time.Hour, func() { close(stopped) })
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a parent that is not ours did not stop the gateway")
	}
}

// TestWatchParent_LeavesALiveParent: the real parent, checked many times,
// never stops it, and the watch ends with its context.
func TestWatchParent_LeavesALiveParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchParent(ctx, os.Getppid(), 10*time.Millisecond, func() { t.Error("stopped with its parent alive") })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived its context")
	}
}

// TestWorkEQ_Classifies: an EntroQ that never answers is worth a restart, and
// a credential that cannot be read is not.
func TestWorkEQ_Classifies(t *testing.T) {
	saved := []any{entroqAddr, entroqStartupTimeout, caFile}
	defer func() {
		entroqAddr = saved[0].(string)
		entroqStartupTimeout = saved[1].(time.Duration)
		caFile = saved[2].(string)
	}()

	for name, tc := range map[string]struct {
		ca   string
		want workgateway.ExitClass
	}{
		"unreachable is transient":    {"", workgateway.ExitTransient},
		"unreadable CA is the caller": {"/nonexistent/ca.pem", workgateway.ExitCaller},
	} {
		t.Run(name, func(t *testing.T) {
			// Port 1 refuses every connection, so the dial runs out its time.
			entroqAddr = "127.0.0.1:1"
			entroqStartupTimeout = 200 * time.Millisecond
			caFile = tc.ca
			var g errgroup.Group
			_, err := workEQ(context.Background(), &g)
			ee, ok := workgateway.AsExit(err)
			if !ok {
				t.Fatalf("workEQ = %v, which carries no class", err)
			}
			if ee.Class != tc.want {
				t.Errorf("class = %v, want %v (%v)", ee.Class, tc.want, err)
			}
		})
	}
}

// TestServeWork_StopsDuringStartup: a signal while EntroQ is still being
// dialed stops the gateway at once and cleanly, in either mode. There is no
// session yet, so a drain has nothing to wait for.
func TestServeWork_StopsDuringStartup(t *testing.T) {
	saved := []any{entroqAddr, entroqStartupTimeout, workLease}
	defer func() {
		entroqAddr = saved[0].(string)
		entroqStartupTimeout = saved[1].(time.Duration)
		workLease = saved[2].(time.Duration)
	}()
	// Port 1 refuses every connection, and both bounds are far longer than
	// the test allows, so only the signal can end the dial in time.
	entroqAddr = "127.0.0.1:1"
	entroqStartupTimeout = time.Minute
	workLease = time.Minute

	for name, serve := range map[string]func() error{
		"stdio": func() error { return serveWorkStdio(discardOut(), nil) },
		"http":  func() error { return serveWorkHTTP("127.0.0.1:0", discardOut(), nil) },
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- serve() }()
			time.Sleep(200 * time.Millisecond)
			if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatalf("signal: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve = %v, want a clean stop", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a signal during start-up did not stop the gateway")
			}
		})
	}
}

// discardOut is a workOut nobody reads.
func discardOut() *workOut {
	return &workOut{w: nopWriteCloser{io.Discard}}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// TestServeWork_SaysWhyItCouldNotStart is a gateway that cannot reach EntroQ:
// before it exits, it writes the reason on stdout as the one line a client
// reads, an error of the class its exit code carries.
func TestServeWork_SaysWhyItCouldNotStart(t *testing.T) {
	saved := []any{entroqAddr, entroqStartupTimeout}
	defer func() {
		entroqAddr = saved[0].(string)
		entroqStartupTimeout = saved[1].(time.Duration)
	}()
	entroqAddr = "127.0.0.1:1"
	entroqStartupTimeout = 200 * time.Millisecond

	for name, serve := range map[string]func(*workOut) error{
		"stdio": func(out *workOut) error { return serveWorkStdio(out, nil) },
		"http":  func(out *workOut) error { return serveWorkHTTP("127.0.0.1:0", out, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			out := &workOut{w: nopWriteCloser{&buf}}
			err := out.report(serve(out))
			if ee, ok := workgateway.AsExit(err); !ok || ee.Class != workgateway.ExitTransient {
				t.Fatalf("serve = %v, want a transient exit", err)
			}
			var line workgateway.Response
			dec := json.NewDecoder(&buf)
			if err := dec.Decode(&line); err != nil {
				t.Fatalf("decode stdout %q: %v", buf.String(), err)
			}
			if line.Type != workgateway.RespError || line.Class != workgateway.ExitTransient.String() || line.Message == "" {
				t.Errorf("stdout line = %+v, want a transient error with a message", line)
			}
			if dec.More() {
				t.Errorf("stdout has more than one line: %q", buf.String())
			}
		})
	}
}

// TestWorkOut_StaysQuietOnceTaken is a failure after a client owns stdout: the
// stream is the client's, so nothing is added to it.
func TestWorkOut_StaysQuietOnceTaken(t *testing.T) {
	var buf bytes.Buffer
	out := &workOut{w: nopWriteCloser{&buf}}
	out.take()
	want := errors.New("after the handover")
	if err := out.report(want); err != want {
		t.Errorf("report = %v, want %v", err, want)
	}
	if buf.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", buf.String())
	}
}

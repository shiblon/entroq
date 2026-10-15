package cmd

import (
	"context"
	"os"
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

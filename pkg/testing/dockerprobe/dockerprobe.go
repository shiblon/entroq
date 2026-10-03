// Package dockerprobe reports whether a Docker daemon is usable, for test
// packages whose subject runs in a testcontainer. Without Docker there is
// nothing for them to test against, and detecting that lets TestMain skip
// cleanly (exit 0) rather than hard-failing the package on a bare
// `go test ./...` or on a CI without a Docker service.
//
// It lives in its own package so that the testcontainers dependency stays out
// of the import graph of the suites that do not need it.
package dockerprobe

import (
	"context"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// pingTimeout bounds the daemon check. An unreachable or black-hole endpoint
// must not hang a TestMain.
const pingTimeout = 10 * time.Second

// Available reports whether a Docker daemon is reachable and answering.
//
// It covers three states, which need three different mechanisms. A daemon that
// is absent fails the constructor. One that is unreachable does not: the
// constructor swallows the probe and hands back an env-derived client, so the
// ping is the real check. And one that is present but answering badly -- a
// wedged Docker Desktop returning 500 from /version, say -- makes the
// constructor PANIC, because testcontainers derives the host through
// MustExtractDockerHost. Recovering that is what keeps a broken daemon
// reported as "skip" rather than as a failing package, which otherwise reads
// as the code being at fault.
func Available(ctx context.Context) bool {
	return available(ctx, ping)
}

// available is Available with its probe injected, so the recover can be tested
// without a daemon. testcontainers memoizes host resolution in a sync.Once, so
// DOCKER_HOST cannot be varied within one process to stage a broken one.
func available(ctx context.Context, probe func(context.Context) error) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return probe(ctx) == nil
}

// ping builds a client and asks the daemon to answer.
func ping(ctx context.Context) error {
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	_, err = cli.Ping(ctx)
	return err
}

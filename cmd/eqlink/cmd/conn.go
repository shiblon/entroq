package cmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// openEntroq opens an EntroQ instance at addr with optional transport security
// (certF/keyF/caF) and bearer-token auth (tokenFile); label names it in errors
// and logs (e.g. "local", "from", "to"). When a token file is given, the token
// is reloaded on rotation (SIGHUP or mtime change) by a goroutine spawned on g,
// and the token's subject becomes the client's claimant.
//
// It backs both the single-instance commands (via localEQ) and "eqlink handoff",
// which opens two instances symmetrically, so neither endpoint is privileged.
func openEntroq(ctx context.Context, g *errgroup.Group, label, addr, certF, keyF, caF, tokenFile string) (*entroq.EntroQ, error) {
	tlsCfg, err := loadTLSConfig(certF, keyF, caF)
	if err != nil {
		return nil, fmt.Errorf("%s tls: %w", label, err)
	}
	var opts []eqgrpc.Option
	if tlsCfg != nil {
		opts = append(opts, eqgrpc.WithDialOpts(grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))))
	} else {
		opts = append(opts, eqgrpc.WithInsecure())
	}

	var eqOpts []entroq.Option
	if tokenFile != "" {
		creds, err := newTokenFileCreds(tokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, eqgrpc.WithDialOpts(grpc.WithPerRPCCredentials(creds)))
		eqOpts = append(eqOpts, entroq.WithClaimantID(creds.claimant))
		watchTokenReload(ctx, g, creds)
	}

	eq, err := entroq.New(ctx, eqgrpc.Opener(addr, opts...), eqOpts...)
	if err != nil {
		return nil, fmt.Errorf("%s entroq: %w", label, err)
	}
	return eq, nil
}

// localEQ opens the local EntroQ instance (--entroq) with optional transport
// security (--cert/--key/--ca) and bearer-token auth (--authz-token-file). It is
// used by the single-instance sidecar commands (send/recv/run/gc); "eqlink
// handoff" opens its two endpoints directly via openEntroq.
func localEQ(ctx context.Context, g *errgroup.Group) (*entroq.EntroQ, error) {
	return openEntroq(ctx, g, "local", entroqAddr, certFile, keyFile, caFile, authzTokenFile)
}

// remoteTLS builds the TLS config for a remote connection (label names it, e.g.
// "source" or "dest"). When none of the remote's cert/key/ca are set, it
// inherits the local --cert/--key/--ca -- a convenience for the common
// single-trust-domain case, where the flags would otherwise be repeated. This
// only ever makes the remote MORE secure (an unset remote is otherwise
// insecure); if the remote lives in a distinct trust domain, set its own
// --<label>-cert/key/ca and no fallback happens. The fallback is logged so it's
// never a surprise.
func remoteTLS(label, cert, key, ca string) (*tls.Config, error) {
	if cert == "" && key == "" && ca == "" {
		if certFile != "" || keyFile != "" || caFile != "" {
			log.Printf("%s: no --%s-cert/--%s-key/--%s-ca set; inheriting local --cert/--key/--ca", label, label, label, label)
		}
		return loadTLSConfig(certFile, keyFile, caFile)
	}
	return loadTLSConfig(cert, key, ca)
}

// tokenFileCreds implements grpc.PerRPCCredentials with an in-memory bearer
// token that is loaded at startup and reloaded on rotation.
type tokenFileCreds struct {
	path     string
	claimant string // sub#nonce, computed once at startup
	token    atomic.Pointer[string]
}

// newTokenFileCreds loads the token immediately; returns an error if the file
// cannot be read.
func newTokenFileCreds(path string) (*tokenFileCreds, error) {
	c := &tokenFileCreds{path: path}
	if err := c.reload(); err != nil {
		return nil, err
	}
	c.claimant = entroq.MustClaimantFromSub(*c.token.Load())
	return c, nil
}

func (c *tokenFileCreds) reload() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("bearer token file %q: %w", c.path, err)
	}
	tok := strings.TrimSpace(string(data))
	c.token.Store(&tok)
	return nil
}

func (c *tokenFileCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + *c.token.Load()}, nil
}

func (*tokenFileCreds) RequireTransportSecurity() bool {
	return false
}

// watchTokenReload spawns a goroutine on g that reloads creds when its token
// file changes -- on SIGHUP, or when the file's mtime advances (polled at
// tokenReloadInterval). k8s projected tokens are rotated by the kubelet by
// rewriting the file. The goroutine returns when ctx is done.
func watchTokenReload(ctx context.Context, g *errgroup.Group, creds *tokenFileCreds) {
	interval := tokenReloadInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	hupsigs := make(chan os.Signal, 1)
	signal.Notify(hupsigs, syscall.SIGHUP)
	g.Go(func() error {
		defer signal.Stop(hupsigs)
		info, _ := os.Stat(creds.path)
		var lastMtime time.Time
		if info != nil {
			lastMtime = info.ModTime()
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-hupsigs:
				if err := creds.reload(); err != nil {
					log.Printf("token reload (SIGHUP): %v", err)
				} else {
					log.Printf("token reloaded (SIGHUP) from %s", creds.path)
				}
			case <-ticker.C:
				fi, err := os.Stat(creds.path)
				if err != nil {
					log.Printf("token stat %s: %v", creds.path, err)
					continue
				}
				if fi.ModTime().After(lastMtime) {
					lastMtime = fi.ModTime()
					if err := creds.reload(); err != nil {
						log.Printf("token reload (stat): %v", err)
					} else {
						log.Printf("token reloaded (stat) from %s", creds.path)
					}
				}
			}
		}
	})
}

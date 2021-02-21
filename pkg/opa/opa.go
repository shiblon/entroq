// Package opa implements utilities for doing authorization using the Open
// Policy Agent (OPA). It uses the authz proto definitions.
package opa

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"

	pb "entrogo.com/entroq/proto"
)

// OPA is a client-like object for interacting with OPA authorization policies.
type OPA struct {
	sync.Mutex

	policyGetter  PolicyGetter
	policyRefresh time.Duration

	cachedCompiler *ast.Compiler

	cancelWatcher context.CancelFunc
}

func un(locker sync.Locker) {
	locker.Unlock()
}

func lock(locker sync.Locker) sync.Locker {
	locker.Lock()
	return locker
}

// New creates a new OPA with the given options. A policy should be specified
// so that it can do its work. This should be closed when no longer in use.
func New(opts ...Option) (*OPA, error) {
	a := new(OPA)
	for _, opt := range opts {
		opt(a)
	}

	if a.policyGetter == nil && a.cachedCompiler == nil {
		return nil, fmt.Errorf("invalid OPA configuration---at least a policy or policy getter must be specified")
	}

	// If there is no refresh policy, don't kick off any refresh things.
	if a.policyRefresh == 0 {
		return a, nil
	}

	// There is a getter and a refresh policy, start a policy refresh watcher.

	// Note that we are using a background context because this is truly a
	// background process. Normally this is the wrong thing to do, but here is
	// is correct.
	ctx := context.Background()
	ctx, a.cancelWatcher = context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(a.policyRefresh):
				refreshContext, _ := context.WithTimeout(ctx, 30*time.Second)
				if err := a.refreshPolicy(refreshContext); err != nil {
					log.Fatalf("Failed to refresh authorization policy: %v", err)
				}
			case <-time.After(a.policyRefresh + time.Minute):
				// If we hit this case, then the refresh context failed to
				// trigger an exit. This is a sort of watchdog case in case of
				// bad getter implementations.
				log.Fatal("Refresh took longer than expected and context failed to trigger an exit.")
			}
		}
	}()

	return a, nil
}

// Close shuts down policy refresh watchers, and releases OPA resources. Should
// be called if the OPA does not share a life cycle with the main process.
func (a *OPA) Close() error {
	if a.cancelWatcher != nil {
		a.cancelWatcher()
	}
	return nil
}

// Option defines creation options for OPA.
type Option func(*OPA) error

// PolicyGetter is passed as a construction option for the queue service if an
// OPA policy should be executed with each request. The getter provides the
// actual policy content (Rego file) every time.
type PolicyGetter func(ctx context.Context) (string, error)

// WithPolicyGetter sets this service up to use the provided getter to
// obtain Rego policy and to note whether it should be reloaded.
// If the interval is 0, it means "never refresh, just get the policy once".
func WithPolicyGetter(getter PolicyGetter, interval time.Duration) Option {
	return func(a *OPA) error {
		a.policyGetter = getter
		a.policyRefresh = interval
		return nil
	}
}

// WithPolicy sets the policy, and it is never changed. String contents of a Rego file.
func WithPolicy(contents string) Option {
	return func(a *OPA) error {
		defer un(lock(a))
		var err error
		if a.cachedCompiler, err = a.compileModule(contents); err != nil {
			return fmt.Errorf("set policy: %w", err)
		}
		return nil
	}
}

// Authorize returns an appropriate Authz Response based on information in the
// context, OPA policy, and request inputs.
func (a *OPA) Authorize(ctx context.Context, req *pb.AuthzRequest) (*pb.AuthzResponse, error) {
	compiler, err := a.regoCompiler(ctx)
	if err != nil {
		return nil, fmt.Errorf("authorize OPA: %w", err)
	}

	query := rego.New(
		rego.Compiler(compiler),
		// TODO: query?
		rego.Input(req),
	)

	results, err := query.Eval(ctx)
	if err != nil {
		return nil, fmt.Errorf("authorize OPA query: %w", err)
	}

	log.Printf("Results: %+v", results)

	authzResp := new(pb.AuthzResponse)

	// TODO: go through the results, get waht we need to create the right kind of error (or not).

	return authzResp, nil
}
func (a *OPA) compileModule(module string) (*ast.Compiler, error) {
	return ast.CompileModules(map[string]string{"entroq.rego": module})
}

func (a *OPA) refreshPolicy(ctx context.Context) error {
	// No refresh if there's no getter.
	if a.policyGetter == nil {
		return nil
	}

	module, err := a.policyGetter(ctx)
	if err != nil {
		return fmt.Errorf("refresh policy: %w", err)
	}

	compiler, err := a.compileModule(module)
	if err != nil {
		return fmt.Errorf("refresh policy: %w", err)
	}

	defer un(lock(a))
	a.cachedCompiler = compiler

	return nil
}

func (a *OPA) regoCompiler(ctx context.Context) (*ast.Compiler, error) {
	defer un(lock(a))
	if a.cachedCompiler != nil {
		return a.cachedCompiler, nil
	}
	if err := a.refreshPolicy(ctx); err != nil {
		return nil, fmt.Errorf("get compiler with first refresh: %w", err)
	}
	return a.cachedCompiler, nil
}

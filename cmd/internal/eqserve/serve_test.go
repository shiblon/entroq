package eqserve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel/metric"
)

func TestBindFlagsDefaults(t *testing.T) {
	var cfg Config
	flags := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	cfg.BindFlags(flags)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}

	if cfg.Port != 37706 || cfg.HTTPPort != 9100 || cfg.MaxSize != 10 {
		t.Fatalf("unexpected transport defaults: %+v", cfg)
	}
	if cfg.AuthzStrategy != "none" {
		t.Fatalf("authz default = %q, want none", cfg.AuthzStrategy)
	}
	if cfg.AuthnStrategy != "none" {
		t.Fatalf("authn default = %q, want none", cfg.AuthnStrategy)
	}
	if cfg.AuthTokenCacheTTL != 30*time.Second || cfg.AuthTokenCacheEntries != 4096 {
		t.Fatalf("unexpected authentication cache defaults: %+v", cfg)
	}
	if cfg.MeshPolicyFile != "" || cfg.MeshUpdateSubject != "" {
		t.Fatalf("unexpected mesh authorization defaults: %+v", cfg)
	}
}

func TestServerKeepalivePolicyAcceptsDefaultClient(t *testing.T) {
	policy := serverKeepalivePolicy()
	if policy.MinTime > eqgrpc.DefaultKeepaliveTime {
		t.Fatalf("server minimum ping interval %v exceeds client interval %v",
			policy.MinTime, eqgrpc.DefaultKeepaliveTime)
	}
	if policy.PermitWithoutStream {
		t.Fatal("server permits keepalive without an active RPC")
	}
}

func TestRunRejectsUnknownAuthorizationBeforeOpeningBackend(t *testing.T) {
	opened := false
	err := Run(context.Background(), Config{AuthzStrategy: "mystery"},
		func(metric.MeterProvider) entroq.BackendOpener {
			opened = true
			return nil
		},
		"test",
	)
	if err == nil || !strings.Contains(err.Error(), `unknown authz strategy: "mystery"`) {
		t.Fatalf("Run error = %v, want unknown strategy", err)
	}
	if opened {
		t.Fatal("backend opener factory called after authorization validation failed")
	}
}

func TestRunRejectsIncompleteSecurityBoundaryBeforeOpeningBackend(t *testing.T) {
	for _, cfg := range []Config{
		{AuthnStrategy: "jwt", AuthzStrategy: "none"},
		{AuthnStrategy: "none", AuthzStrategy: "opahttp"},
	} {
		opened := false
		err := Run(context.Background(), cfg,
			func(metric.MeterProvider) entroq.BackendOpener {
				opened = true
				return nil
			},
			"test",
		)
		if err == nil {
			t.Fatalf("Run(%+v) accepted an incomplete security boundary", cfg)
		}
		if opened {
			t.Fatal("backend opener called after security configuration failed")
		}
	}
}

func TestRunRequiresMeshUpdateSubjectBeforeOpeningBackend(t *testing.T) {
	opened := false
	err := Run(context.Background(), Config{
		AuthnStrategy: "jwt",
		AuthzStrategy: "mesh",
	}, func(metric.MeterProvider) entroq.BackendOpener {
		opened = true
		return nil
	}, "test")
	if err == nil || !strings.Contains(err.Error(), "mesh update subject is required") {
		t.Fatalf("Run error = %v, want missing mesh update subject", err)
	}
	if opened {
		t.Fatal("backend opener called after incomplete mesh configuration")
	}
}

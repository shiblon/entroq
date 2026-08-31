package eqserve

import (
	"context"
	"strings"
	"testing"

	"github.com/shiblon/entroq"
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

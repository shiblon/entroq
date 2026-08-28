package eqmem

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	backendbench "github.com/shiblon/entroq/pkg/testing/benchmark"
)

func BenchmarkBackend(b *testing.B) {
	backendbench.RunBackendBenchmarks(b, "memory", func(ctx context.Context, _ *testing.B) (*entroq.EntroQ, error) {
		return entroq.New(ctx, Opener())
	})
	backendbench.RunBackendBenchmarks(b, "journal", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return entroq.New(ctx, Opener(WithJournal(b.TempDir())))
	})
}

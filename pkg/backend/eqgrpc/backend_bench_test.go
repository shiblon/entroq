package eqgrpc_test

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	backendbench "github.com/shiblon/entroq/pkg/testing/benchmark"
)

func BenchmarkBackend(b *testing.B) {
	backendbench.RunBackendBenchmarks(b, "grpc-memory", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, eqmem.Opener())
	})
	backendbench.RunBackendBenchmarks(b, "grpc-journal", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, eqmem.Opener(eqmem.WithJournal(b.TempDir())))
	})
}

func BenchmarkMapReduceLoad(b *testing.B) {
	backendbench.RunMapReduceLoadBenchmarks(b, "grpc-memory", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, eqmem.Opener())
	})
	backendbench.RunMapReduceLoadBenchmarks(b, "grpc-journal", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, eqmem.Opener(eqmem.WithJournal(b.TempDir())))
	})
}

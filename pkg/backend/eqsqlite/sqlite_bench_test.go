package eqsqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shiblon/entroq"
	backendbench "github.com/shiblon/entroq/pkg/testing/benchmark"
)

func BenchmarkBackend(b *testing.B) {
	backendbench.RunBackendBenchmarks(b, "sqlite", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return entroq.New(ctx, Opener(filepath.Join(b.TempDir(), "entroq.sqlite")))
	})
	backendbench.RunBackendBenchmarks(b, "grpc-sqlite", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, Opener(filepath.Join(b.TempDir(), "entroq.sqlite")))
	})
}

func BenchmarkMapReduceLoad(b *testing.B) {
	backendbench.RunMapReduceLoadBenchmarks(b, "grpc-sqlite", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, Opener(filepath.Join(b.TempDir(), "entroq.sqlite")))
	})
}

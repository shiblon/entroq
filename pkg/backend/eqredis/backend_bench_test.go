package eqredis

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	backendbench "github.com/shiblon/entroq/pkg/testing/benchmark"
)

func BenchmarkBackend(b *testing.B) {
	backendbench.RunBackendBenchmarks(b, "redis", func(ctx context.Context, _ *testing.B) (*entroq.EntroQ, error) {
		return entroq.New(ctx, Opener(WithAddr(redisAddr)))
	})
	backendbench.RunBackendBenchmarks(b, "grpc-redis", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, Opener(WithAddr(redisAddr)))
	})
}

func BenchmarkMapReduceLoad(b *testing.B) {
	backendbench.RunMapReduceLoadBenchmarks(b, "grpc-redis", func(ctx context.Context, b *testing.B) (*entroq.EntroQ, error) {
		return backendbench.OpenGRPC(ctx, b, Opener(WithAddr(redisAddr)))
	})
}

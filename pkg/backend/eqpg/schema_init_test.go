package eqpg

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestInitSchemaConcurrently starts several InitSchema calls at once against
// an empty database, as replicas started together with --init_schema do. Each
// must succeed, and the schema must open afterward.
func TestInitSchemaConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const replicas = 8
	for round := range 3 {
		db := freshDB(ctx, t, fmt.Sprintf("entroq_init_concurrently_%d", round))

		var wg sync.WaitGroup
		errs := make(chan error, replicas)
		for range replicas {
			wg.Go(func() {
				errs <- InitSchema(ctx, db)
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("Round %d: concurrent InitSchema: %v", round, err)
			}
		}

		digest, err := StoredSchemaDigest(ctx, db)
		if err != nil {
			t.Fatalf("Round %d: stored digest: %v", round, err)
		}
		if digest != SchemaDigest {
			t.Fatalf("Round %d: stored digest %q, want %q", round, digest, SchemaDigest)
		}
	}
}

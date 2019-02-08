package mem

import (
	"context"
	"testing"

	"github.com/shiblon/entroq/qsvc/qtest"
)

func TestSimpleSequence(t *testing.T) {
	ctx := context.Background()

	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()

	qtest.SimpleSequence(ctx, t, client, "")
}

func TestSimpleWorker(t *testing.T) {
	ctx := context.Background()

	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()

	qtest.SimpleWorker(ctx, t, client, "")
}

func TestQueueMatch(t *testing.T) {
	ctx := context.Background()

	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()

	qtest.QueueMatch(ctx, t, client, "")
}

package entroq_test

import (
	"context"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqmem"
)

func Example() {
	// Create an in-memory EntroQ instance.
	// Other backends are available, notably eqpg to use Postgres, or eqgrpc to
	// speak to EntroQ as a GRPC client. In the GRPC case, see cmd/eq*svc to
	// start up the server side of a GRPC connection.
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())

	// Queues appear and disappear as tasks are added or removed. They are
	// essentially an ephemeral concept. Names are just text.
	// Task values are []byte and can be basically anything, but size matters:
	// files are best represented as paths, for example.

	// Insert a few tasks:
	ins, _, err := eq.Modify(ctx,
		entroq.InsertingInto("q1", entroq.WithValue([]byte("hello 1"))),
		entroq.InsertingInto("q2", entroq.WithValue([]byte("hello 2"))))
	if err != nil {
		log.Fatalf("Error inserting tasks: %v", err)
	}

	// You can get auto-assigned IDs, versions, etc. from each inserted task:
	for _, t := range ins {
		log.Printf("Task inserted: %s\n", t)
	}

	// Fire up a worker to read them. Workers can listen on one or more queues
	// simultaneously. If more than one queue is specified, it will attempt to
	// read from them uniformly randomly for fairness. This can be used to
	// "wedge in" tasks that need to be handled quickly while larger batches are
	// going elsewhere.
	worker := eq.NewWorker(entroq.FuncHandler(
		// Workers claim a task and pass it to your handler functions. In the
		// background, the task's lease is renewed while the first function runs.
		func(ctx context.Context, initial *entroq.Task) error {
			log.Printf("Worker handling task %s", initial)
			// Do work with it here.
			return nil
		},
		// When ready to commit changes to the task (including deletion), the second
		// function passes the version-stable task after the renewer is stopped,
		// making it safe to use it in modification transactions.
		func(ctx context.Context, final *entroq.Task) error {
			log.Printf("Deleting task %s", final)
			_, _, err := eq.Modify(ctx, final.AsDeletion())
			if err != nil {
				return err
			}
			return nil
		},
	))

	// The worker runs forever, so for the sake of this example we pass it a context
	// and cancel it. Other errors typicall just cause a retry (including timeouts).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { time.Sleep(2 * time.Second); cancel() }()

	log.Fatal(worker.Run(ctx, "q1", "q2"))
}

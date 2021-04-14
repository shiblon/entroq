package eqmem_test

import (
	"context"
	"fmt"
	"log"
	"os"

	"entrogo.com/entroq"
	"entrogo.com/entroq/backend/eqmem"
)

func Example_journal() {
	journalDir, err := os.MkdirTemp("", "eqjournal-")
	if err != nil {
		log.Fatalf("Error opening temp dir for journal: %v", err)
	}
	defer os.RemoveAll(journalDir)

	ctx := context.Background()

	eq, err := entroq.New(ctx, eqmem.Opener(eqmem.WithJournal(journalDir)))
	if err != nil {
		log.Fatalf("Error opening client at dir %q: %v", journalDir, err)
	}

	inserted, _, err := eq.Modify(ctx,
		entroq.InsertingInto("/queue/of/tasks", entroq.WithValue([]byte("hey"))),
		entroq.InsertingInto("/queue/of/others", entroq.WithValue([]byte("other"))),
	)
	if err != nil {
		log.Fatalf("Error adding task: %v", err)
	}

	// Change the queue for the first insertion.
	if _, _, err := eq.Modify(ctx, inserted[0].AsChange(entroq.QueueTo("/queue/of/something"))); err != nil {
		log.Fatalf("Error modifying task: %v", err)
	}

	// Close and reopen, see that everything is still there.
	eq.Close()
	eq = nil

	if eq, err = entroq.New(ctx, eqmem.Opener(eqmem.WithJournal(journalDir))); err != nil {
		log.Fatalf("Error reopening client at dir %q: %v", journalDir, err)
	}

	empty, err := eq.QueuesEmpty(ctx, entroq.MatchExact("/queue/of/tasks"))
	if err != nil {
		log.Fatalf("Error checking for empty queues: %v", err)
	}
	fmt.Printf("Empty: %v\n", empty)

	ts1, err := eq.Tasks(ctx, "/queue/of/others")
	if err != nil {
		log.Fatalf("Error getting tasks for 'others': %v", err)
	}
	for _, t := range ts1 {
		fmt.Printf("%v: %q\n", t.Queue, t.Value)
	}

	ts2, err := eq.Tasks(ctx, "/queue/of/something")
	if err != nil {
		log.Fatalf("Error getting tasks for 'something': %v", err)
	}
	for _, t := range ts2 {
		fmt.Printf("%v: %q\n", t.Queue, t.Value)
	}

	// Output:
	// Empty: true
	// /queue/of/others: "other"
	// /queue/of/something: "hey"
}

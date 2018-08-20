package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/etcd"
	"github.com/shiblon/entroq/pg"

	_ "github.com/lib/pq"
)

func basicTest(ctx context.Context, client *entroq.EntroQ) {
	fmt.Println(client.Queues(ctx))

	claimant := uuid.New()

	fmt.Println("Inserting a task")
	inserts, _, err := client.Modify(ctx, claimant, entroq.InsertingInto("group", entroq.WithValue([]byte("hi"))))
	if err != nil {
		log.Fatalf("Error adding task: %v", err)
	}
	fmt.Println(inserts[0])

	fmt.Println("Claiming a task")
	t, err := client.Claim(ctx, claimant, "group", 10*time.Second)
	if err != nil {
		log.Fatalf("Error claiming task: %v", err)
	}
	fmt.Println(t)

	fmt.Println("Extending a task's claim time")
	_, changes, err := client.Modify(ctx, claimant, entroq.Changing(t, entroq.ArrivalTimeBy(10*time.Second)))
	if err != nil {
		log.Fatalf("Error extending claim time: %v", err)
	}
	fmt.Println(changes[0])

	fmt.Println("Listing tasks in group queue")
	tasks, err := client.Tasks(ctx, "group")
	fmt.Println(tasks)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Etcd Opener
	etcdOpener := etcd.Opener([]string{"localhost:2379", "localhost:2380"})

	// Postgres Opener
	pgOpener := pg.Opener("localhost", "postgres", "postgres", "password", false)

	etcdClient, err := entroq.New(ctx, etcdOpener)
	if err != nil {
		log.Fatalf("Failed to create etcdClient: %v", err)
	}
	defer etcdClient.Close()

	pgClient, err := entroq.New(ctx, pgOpener)
	if err != nil {
		log.Fatalf("Failed to create pgClient: %v", err)
	}
	defer pgClient.Close()

	fmt.Println("etcd:")
	basicTest(ctx, etcdClient)

	fmt.Println("postgres:")
	basicTest(ctx, pgClient)
}

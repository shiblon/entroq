package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/etcd"

	_ "github.com/lib/pq"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Etcd Opener
	opener := func(_ context.Context) (entroq.Backend, error) {
		cli, err := clientv3.NewFromURLs([]string{"localhost:2379", "localhost:2380"})
		if err != nil {
			return nil, fmt.Errorf("failed to open etcd: %v", err)
		}
		return etcd.New(cli)
	}

	/*
		// Postgres Opener
		opener := func(ctx context.Context) (entroq.Backend, error) {
			db, err := sql.Open("postgres", "dbname=entroq user=postgres sslmode=disable password=password")
			if err != nil {
				return nil, fmt.Errorf("error opening database: %v", err)
			}
			return pg.New(ctx, db)
		}
	*/

	client, err := entroq.NewClient(ctx, opener)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	fmt.Println(client.Queues(ctx))

	fmt.Println("Inserting a task")
	inserts, _, err := client.Modify(ctx, entroq.InsertingInto("group", entroq.WithValue([]byte("hi"))))
	if err != nil {
		log.Fatalf("Error adding task: %v", err)
	}
	fmt.Println(inserts[0])

	fmt.Println("Claiming a task")
	t, err := client.Claim(ctx, "group", 10*time.Second)
	if err != nil {
		log.Fatalf("Error claiming task: %v", err)
	}
	fmt.Println(t)

	fmt.Println("Extending a task's claim time")
	_, changes, err := client.Modify(ctx, entroq.Changing(t, entroq.ArrivalTimeBy(10*time.Second)))
	if err != nil {
		log.Fatalf("Error extending claim time: %v", err)
	}
	fmt.Println(changes[0])

	fmt.Println("Listing tasks in group queue")
	tasks, err := client.Tasks(ctx, "group")
	fmt.Println(tasks, entroq.IncludeClaimed())
}

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pg"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.NewClient(ctx,
		pg.Opener(
			pg.WithDBName("entroq"),
			pg.WithUsername("postgres"),
			pg.WithSSLMode("disable"),
			pg.WithPassword("password"),
		),
	)

	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}
	defer client.Close()

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

	tasks, err := client.Tasks(ctx, "group")
	fmt.Println(tasks, entroq.IncludeClaimed())
}

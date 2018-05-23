package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq/pgtask"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := pgtask.NewClient(ctx)
	if err != nil {
		log.Fatalf("Error opening task client: %v", err)
	}
	defer client.Close()

	fmt.Println("Inserting a task")
	inserts, _, err := client.Commit(ctx, pgtask.InsertingInto("group", pgtask.WithValue([]byte("hi"))))
	if err != nil {
		log.Fatalf("Error adding task: %v", err)
	}
	fmt.Println(inserts[0])

	fmt.Println("Claiming a task")
	task, err := client.Claim(ctx, "group", pgtask.For(10*time.Second))
	if err != nil {
		log.Fatalf("Error claiming task: %v", err)
	}
	fmt.Println(task)

	fmt.Println("Extending a task's claim time")
	_, changes, err := client.Commit(ctx, pgtask.Changing(task, pgtask.ArrivalTimeBy(10*time.Second)))
	if err != nil {
		log.Fatalf("Error extending claim time: %v", err)
	}
	fmt.Println(changes[0])

	tasks, err := client.Tasks(ctx, "group")
	fmt.Println(tasks, pgtask.IncludeClaimed())
}

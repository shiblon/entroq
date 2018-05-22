package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq/pgtask"
)

func main() {
	client, err := pgtask.NewClient()
	if err != nil {
		log.Fatalf("Error opening task client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	//fmt.Println(addTask(ctx, db, "group", []byte("a value")))
	fmt.Println(client.ClaimTask(ctx, "group", pgtask.For(1*time.Minute)))
	fmt.Println(client.ClaimTask(ctx, "group", pgtask.For(30*time.Second)))
	fmt.Println(client.ClaimTask(ctx, "group", pgtask.For(5*time.Second)))
}

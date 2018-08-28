// Command svc starts an in-memory task service with gRPC endpoints.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/shiblon/entroq/mem"
	"github.com/shiblon/entroq/qsvc"
	"google.golang.org/grpc"

	pb "github.com/shiblon/entroq/qsvc/proto"
)

var (
	port = flag.Int("port", 37706, "Listening port for EntroQ service")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Error listening on port %d: %v", *port, err)
	}

	svc, err := qsvc.New(ctx, mem.Opener())
	if err != nil {
		log.Fatalf("Failed to open backend for qsvc: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterEntroQServer(s, svc)
	s.Serve(lis)
}

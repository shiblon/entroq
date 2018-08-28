// Command svc starts a basic gRPC etcd-backed task manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	pb "github.com/shiblon/entroq/qsvc/proto"

	"github.com/shiblon/entroq/etcd"
	"github.com/shiblon/entroq/qsvc"
	"google.golang.org/grpc"
)

var (
	port     = flag.Int("port", 37706, "Listening port for EntroQ service")
	backends = flag.Int("backends", 10, "Number of backend connections to maintain")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", *port))

	if err != nil {
		log.Fatalf("Error listening on port %d: %v", *port, err)
	}

	svc, err := qsvc.New(ctx, etcd.Opener(flag.Args()), qsvc.WithConnections(*backends))
	if err != nil {
		log.Fatalf("Failed to open backend for qsvc: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterEntroQServer(s, svc)
	s.Serve(lis)
}

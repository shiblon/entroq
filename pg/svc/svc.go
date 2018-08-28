// Command svc starts a postgres-backed task queue service that can
// be accessed via gRPC calls.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/shiblon/entroq/pg"
	"github.com/shiblon/entroq/qsvc"
	"google.golang.org/grpc"

	pb "github.com/shiblon/entroq/qsvc/proto"

	_ "github.com/lib/pq"
)

var (
	port       = flag.Int("port", 37706, "Listening port for EntroQ service")
	backends   = flag.Int("backends", 10, "Number of backend connections to maintain")
	dbName     = flag.String("dbname", "postgres", "Database name housing tasks.")
	dbUser     = flag.String("dbuser", "postgres", "Database user name.")
	dbPassword = flag.String("dbpwd", "postgres", "Database password.")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", *port))
	if err != nil {
		log.Fatalf("Error listening on port %d: %v", *port, err)
	}

	svc, err := qsvc.New(ctx, pg.Opener(fmt.Sprintf("[::]:%d", *port), *dbName, *dbUser, *dbPassword, false), *backends)
	if err != nil {
		log.Fatalf("Failed to open backend for qsvc: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterEntroQServer(s, svc)
	s.Serve(lis)
}

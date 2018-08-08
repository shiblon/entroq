// Command svc starts a postgres-backed task queue service that can
// be accessed via gRPC calls.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/shiblon/entroq"
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
	lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", *port))
	if err != nil {
		log.Fatalf("Error listening on port %d: %v", *port, err)
	}
	s := grpc.NewServer()

	ctx := context.Background()

	pgOpener := pg.Opener(*dbName, *dbUser, *dbPassword, false)
	var clients []*entroq.EntroQ
	for i := 0; i < *backends; i++ {
		cli, err := entroq.New(ctx, pgOpener)
		if err != nil {
			log.Fatalf("Error making a pg task client: %v", err)
		}
		clients = append(clients, cli)
	}
	pb.RegisterEntroQServer(s, qsvc.New(clients))
	s.Serve(lis)
}

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
	"google.golang.org/grpc/health"

	pb "github.com/shiblon/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"

	_ "github.com/lib/pq"
)

var (
	port       = flag.Int("port", 37706, "Listening port for EntroQ service")
	dbAddr     = flag.String("dbaddr", "localhost:5432", "Database host address.")
	dbName     = flag.String("dbname", "postgres", "Database name housing tasks.")
	dbUser     = flag.String("dbuser", "postgres", "Database user name.")
	dbPassword = flag.String("dbpwd", "postgres", "Database password.")
	attempts   = flag.String("attempts", 5, "Number of connection attempts before failure (5 seconds in between).")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	svc, err := qsvc.New(ctx, pg.Opener(*dbAddr,
		pg.WithDB(*dbName),
		pg.WithUsername(*dbUser),
		pg.WithPassword(*dbPassword),
		pg.WithConnectAttempts(*attempts),
	))
	if err != nil {
		log.Fatalf("Failed to open backend for qsvc: %v", err)
	}
	defer svc.Close()

	lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", *port))
	if err != nil {
		log.Fatalf("Error listening on port %d: %v", *port, err)
	}

	s := grpc.NewServer()
	pb.RegisterEntroQServer(s, svc)
	hpb.RegisterHealthServer(s, health.NewServer())
	s.Serve(lis)
}

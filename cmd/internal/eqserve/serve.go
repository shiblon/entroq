// Package eqserve provides the common transport shell for EntroQ backend
// service commands.
package eqserve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/authz/opahttp"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/eqsvcjson"
	"github.com/shiblon/entroq/pkg/otel"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const bytesPerMB = 1024 * 1024

// Config holds the transport settings shared by backend service commands.
type Config struct {
	Port     int
	HTTPPort int
	MaxSize  int

	AuthzStrategy string
	OPAURL        string
	OPAPath       string

	MetricInterval time.Duration
}

// BindFlags adds the common service flags to f.
func (c *Config) BindFlags(f *pflag.FlagSet) {
	f.IntVar(&c.Port, "port", 37706, "gRPC service port.")
	f.IntVar(&c.HTTPPort, "http_port", 9100, "HTTP port for /metrics and JSON/Connect API.")
	f.IntVar(&c.MaxSize, "max_size_mb", 10, "Maximum gRPC message size in MB (send and receive).")
	f.StringVar(&c.AuthzStrategy, "authz", "none", "Authorization strategy: none, opahttp.")
	f.StringVar(&c.OPAURL, "opa_url", "", fmt.Sprintf("OPA base URL. Default: %s.", opahttp.DefaultHostURL))
	f.StringVar(&c.OPAPath, "opa_path", "", fmt.Sprintf("OPA API path. Default: %s.", opahttp.DefaultAPIPath))
}

// OpenFunc constructs a backend opener after telemetry has been initialized.
type OpenFunc func(metric.MeterProvider) entroq.BackendOpener

// Run exposes a backend over gRPC and HTTP/JSON until a server fails or ctx is
// canceled. Backend-specific commands retain responsibility for validating
// their own flags and constructing open.
func Run(ctx context.Context, cfg Config, open OpenFunc, backendDescription string) error {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	authzOpt, err := authorizationOption(cfg)
	if err != nil {
		return err
	}

	mp, metricsHandler, stopMetrics, err := otel.NewPrometheusProvider()
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer stopMetrics()

	svcOpts := []eqsvcgrpc.Option{
		authzOpt,
		eqsvcgrpc.WithMeterProvider(mp),
	}
	if cfg.MetricInterval > 0 {
		svcOpts = append(svcOpts, eqsvcgrpc.WithMetricInterval(cfg.MetricInterval))
	}

	svc, err := eqsvcgrpc.New(ctx, open(mp), svcOpts...)
	if err != nil {
		return fmt.Errorf("open %s backend: %w", backendDescription, err)
	}
	defer svc.Close()

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler)
	path, handler, err := eqsvcjson.New(svc)
	if err != nil {
		return fmt.Errorf("create JSON/Connect handler: %w", err)
	}
	mux.Handle(path, handler)

	grpcListener, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("listen on gRPC port %d: %w", cfg.Port, err)
	}
	defer grpcListener.Close()

	httpListener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.HTTPPort))
	if err != nil {
		return fmt.Errorf("listen on HTTP port %d: %w", cfg.HTTPPort, err)
	}
	defer httpListener.Close()

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(cfg.MaxSize*bytesPerMB),
		grpc.MaxSendMsgSize(cfg.MaxSize*bytesPerMB),
	)
	pb.RegisterEntroQServer(grpcServer, svc)
	hpb.RegisterHealthServer(grpcServer, health.NewServer())

	httpServer := &http.Server{Handler: mux}
	errs := make(chan error, 2)
	go func() {
		if err := httpServer.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("HTTP server: %w", err)
		}
	}()
	go func() {
		if err := grpcServer.Serve(grpcListener); err != nil {
			errs <- fmt.Errorf("gRPC server: %w", err)
		}
	}()

	log.Printf("Starting EntroQ gRPC server %s -> %s", grpcListener.Addr(), backendDescription)
	log.Printf("Starting EntroQ HTTP/JSON and metrics server %s", httpListener.Addr())

	select {
	case err := <-errs:
		grpcServer.Stop()
		_ = httpServer.Close()
		return err
	case <-ctx.Done():
		grpcServer.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		return nil
	}
}

func authorizationOption(cfg Config) (eqsvcgrpc.Option, error) {
	switch cfg.AuthzStrategy {
	case "opahttp":
		return eqsvcgrpc.WithAuthorizer(opahttp.New(
			opahttp.WithHostURL(cfg.OPAURL),
			opahttp.WithAPIPath(cfg.OPAPath),
		)), nil
	case "", "none":
		return eqsvcgrpc.WithAuthorizer(nil), nil
	default:
		return nil, fmt.Errorf("unknown authz strategy: %q", cfg.AuthzStrategy)
	}
}

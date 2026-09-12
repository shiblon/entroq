package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/shiblon/entroq/pkg/version"
	"github.com/shiblon/entroq/pkg/workgateway"
	"github.com/spf13/cobra"
)

var (
	entroqAddr           string
	entroqStartupTimeout time.Duration
	authzTokenFile       string

	certFile string
	keyFile  string
	caFile   string
)

var rootCmd = &cobra.Command{
	Use:     "eqlink",
	Version: version.Version,
	Short:   "Experimental HTTP networking sidecar over EntroQ task queues.",
	Long: `EXPERIMENTAL: eqlink's protocol and command surface may change without compatibility.

eqlink translates HTTP calls into EntroQ task queue operations, giving services
decoupled addressing and queue-based load distribution without changing their code.

The sender proxies outgoing HTTP calls from the local service into queues.
The receiver claims tasks from queues and forwards them to the local service.
HTTP bodies are carried as arbitrary byte segments over independent request and
response lane pairs. This supports HTTP/1.1 response streaming and concurrent
HTTP/2 request/response streaming, including trailers, without parsing SSE,
NDJSON, or gRPC messages. Each segment pays a queue round trip, so streaming is
best suited to low-rate status and compatibility traffic. Protocol upgrades such
as WebSocket are not supported. Quiet lanes exchange empty heartbeats; three
missed heartbeat intervals end the session. Stale session queues are
garbage-collected by the EntroQ server, not the sidecar.

The handoff command links two EntroQ instances directly, claiming tasks from a
source instance and delivering them into a destination instance, exactly once.`,
}

// Execute is the entry point called from main. A gateway that stops with a
// classified *ExitError exits with the class's code (sysexits conventions) so a
// supervisor can tell a transient backend blip from a caller fault; anything
// else is a generic failure (exit 1).
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if ee, ok := workgateway.AsExit(err); ok {
			os.Exit(ee.Class.ExitCode())
		}
		os.Exit(1)
	}
}

func init() {
	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&entroqAddr, "entroq", "localhost:37706", "EntroQ gRPC service address.")
	pflags.DurationVar(&entroqStartupTimeout, "entroq-startup-timeout", 30*time.Second, "How long to wait for EntroQ to become healthy at startup; must be positive.")
	pflags.StringVar(&authzTokenFile, "authz-token-file", "", "Path to a bearer token file. Read per-RPC; handles k8s projected token rotation.")

	pflags.StringVar(&certFile, "cert", "", "Path to the TLS certificate file.")
	pflags.StringVar(&keyFile, "key", "", "Path to the TLS private key file.")
	pflags.StringVar(&caFile, "ca", "", "Path to the CA bundle for verifying peers.")
}

func loadTLSConfig(certF, keyF, caF string) (*tls.Config, error) {
	if certF == "" && keyF == "" && caF == "" {
		return nil, nil
	}

	if (certF != "") != (keyF != "") {
		return nil, fmt.Errorf("both --cert and --key must be provided together")
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if certF != "" {
		cert, err := tls.LoadX509KeyPair(certF, keyF)
		if err != nil {
			return nil, fmt.Errorf("load keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	if caF != "" {
		caData, err := os.ReadFile(caF)
		if err != nil {
			return nil, fmt.Errorf("read ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("failed to append CA certs")
		}
		cfg.RootCAs = pool
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return cfg, nil
}

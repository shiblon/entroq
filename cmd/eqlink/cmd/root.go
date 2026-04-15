package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/shiblon/entroq/pkg/version"
	"github.com/spf13/cobra"
)

var (
	entroqAddr string

	certFile string
	keyFile  string
	caFile   string
)

var rootCmd = &cobra.Command{
	Use:     "eqlink",
	Version: version.Version,
	Short:   "Async HTTP networking sidecar over EntroQ task queues.",
	Long: `eqlink translates synchronous HTTP calls into EntroQ task queue operations,
giving services fault tolerance and load balancing without changing their code.

The sender proxies outgoing HTTP calls from the local service into queues.
The receiver claims tasks from queues and forwards them to the local service.
The GC cleans up stale response queues left by crashed senders.`,
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&entroqAddr, "entroq", "localhost:37706", "EntroQ gRPC service address.")

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

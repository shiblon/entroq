package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/authz/opahttp"
)

type authorizer interface {
	Authorize(context.Context, *authz.Request) error
}

type authProxyConfig struct {
	Namespace    string
	DomainSuffix string
	Service      string
	BearerToken  string
	Authorizer   authorizer
	Upstream     http.Handler
	LogDenials   bool
}

func authProxy(args []string) error {
	flags := flag.NewFlagSet("auth-proxy", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "HTTP listen address")
	upstreamURL := flags.String("upstream-url", "", "HTTP service receiving authorized requests")
	service := flags.String("service", "", "single service name hosted by this proxy")
	namespace := flags.String("namespace", "", "namespace prepended to the service queue")
	domainSuffix := flags.String("domain-suffix", ".localhost", "Host suffix used to derive the target service")
	tokenFile := flags.String("authz-token-file", "", "file containing the bearer token presented to OPA")
	opaURL := flags.String("opa-url", opahttp.DefaultHostURL, "OPA base URL")
	opaPath := flags.String("opa-path", opahttp.DefaultAPIPath, "OPA authorization decision path")
	requestTimeout := flags.Duration("request-timeout", 10*time.Second, "upstream request timeout")
	maxBody := flags.Int64("max-body-bytes", 1<<20, "largest accepted request body")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *upstreamURL == "" || *service == "" || *namespace == "" || *tokenFile == "" {
		return fmt.Errorf("upstream-url, service, namespace, and authz-token-file are required")
	}
	if *requestTimeout <= 0 || *maxBody < 1 {
		return fmt.Errorf("request-timeout and max-body-bytes must be positive")
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("read authz token: %w", err)
	}

	upstream := serveHandler(serveConfig{
		MaxBody:     *maxBody,
		UpstreamURL: *upstreamURL,
		Client:      &http.Client{Timeout: *requestTimeout},
	})
	handler := authProxyHandler(authProxyConfig{
		Namespace:    *namespace,
		DomainSuffix: *domainSuffix,
		Service:      *service,
		BearerToken:  strings.TrimSpace(string(token)),
		Authorizer: opahttp.New(
			opahttp.WithHostURL(*opaURL),
			opahttp.WithAPIPath(*opaPath),
		),
		Upstream:   upstream,
		LogDenials: true,
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("auth proxy shutdown: %v", err)
		}
	}()

	log.Printf("serving authorized direct benchmark path on %s -> %s", *addr, *upstreamURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("auth proxy: %w", err)
	}
	return nil
}

func authProxyHandler(config authProxyConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /work", func(w http.ResponseWriter, r *http.Request) {
		service, queue, err := directTarget(r.Host, config.DomainSuffix, config.Namespace)
		if err != nil {
			http.Error(w, "invalid target service", http.StatusBadRequest)
			return
		}
		req := &authz.Request{
			Authz: authz.NewHeaderAuthorization("Bearer " + config.BearerToken),
			Queues: []*authz.Queue{{
				Exact:   queue,
				Actions: []authz.Action{authz.Insert},
			}},
		}
		if err := config.Authorizer.Authorize(r.Context(), req); err != nil {
			if config.LogDenials {
				log.Printf("direct authorization denied for %s: %v", queue, err)
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if service != config.Service {
			http.Error(w, "service not hosted by this proxy", http.StatusNotFound)
			return
		}
		config.Upstream.ServeHTTP(w, r)
	})
	return mux
}

func directTarget(host, domainSuffix, namespace string) (service, queue string, err error) {
	if parsed, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = parsed
	} else if strings.Count(host, ":") == 1 {
		host, _, _ = strings.Cut(host, ":")
	}
	if domainSuffix == "" || !strings.HasSuffix(host, domainSuffix) {
		return "", "", fmt.Errorf("host %q does not end with %q", host, domainSuffix)
	}
	service = strings.TrimSuffix(host, domainSuffix)
	if service == "" || strings.Contains(service, ".") {
		return "", "", fmt.Errorf("host %q does not name one service", host)
	}
	return service, path.Join("/", namespace, service, "inbox"), nil
}

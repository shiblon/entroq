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

	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authn/jwtauthn"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/authz/opahttp"
)

type authenticator interface {
	Authenticate(context.Context, *authn.Credentials) (*authn.VerifiedPrincipal, error)
}

type authorizer interface {
	Authorize(context.Context, *authz.Request) error
}

type authProxyConfig struct {
	Namespace     string
	DomainSuffix  string
	Service       string
	Credentials   *authn.Credentials
	Authenticator authenticator
	Authorizer    authorizer
	Upstream      http.Handler
	LogDenials    bool
}

func authProxy(args []string) error {
	flags := flag.NewFlagSet("auth-proxy", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "HTTP listen address")
	upstreamURL := flags.String("upstream-url", "", "HTTP service receiving authorized requests")
	service := flags.String("service", "", "single service name hosted by this proxy")
	namespace := flags.String("namespace", "", "namespace prepended to the service queue")
	domainSuffix := flags.String("domain-suffix", ".localhost", "Host suffix used to derive the target service")
	tokenFile := flags.String("authz-token-file", "", "file containing the bearer token verified before authorization")
	authJWKSURL := flags.String("auth-jwks-url", "https://kubernetes.default.svc/openid/v1/jwks", "JWKS URL used to verify JWT signatures")
	authIssuer := flags.String("auth-issuer", "https://kubernetes.default.svc.cluster.local", "required JWT issuer")
	authAudience := flags.String("auth-audience", "https://kubernetes.default.svc.cluster.local", "required JWT audience")
	authCAFile := flags.String("auth-ca-file", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "additional CA certificate file for JWKS HTTPS")
	authTokenCacheTTL := flags.Duration("auth-token-cache-ttl", 30*time.Second, "maximum lifetime of a cached verified JWT")
	authTokenCacheEntries := flags.Int("auth-token-cache-entries", 4096, "maximum number of cached verified JWTs")
	authJWKSCacheTTL := flags.Duration("auth-jwks-cache-ttl", 5*time.Minute, "lifetime of cached JWKS key material")
	authJWKSRefreshInterval := flags.Duration("auth-jwks-refresh-interval", 5*time.Second, "minimum interval between unknown-key JWKS refreshes")
	authHTTPTimeout := flags.Duration("auth-http-timeout", 10*time.Second, "timeout for JWKS HTTP requests")
	opaURL := flags.String("opa-url", opahttp.DefaultHostURL, "OPA base URL")
	opaPath := flags.String("opa-path", opahttp.DefaultAPIPath, "OPA authorization decision path")
	requestTimeout := flags.Duration("request-timeout", 10*time.Second, "upstream request timeout")
	maxBody := flags.Int64("max-body-bytes", 1<<20, "largest accepted request body")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *upstreamURL == "" || *service == "" || *namespace == "" || *tokenFile == "" ||
		*authJWKSURL == "" || *authIssuer == "" || *authAudience == "" {
		return fmt.Errorf("upstream-url, service, namespace, authz-token-file, auth-jwks-url, auth-issuer, and auth-audience are required")
	}
	if *requestTimeout <= 0 || *maxBody < 1 {
		return fmt.Errorf("request-timeout and max-body-bytes must be positive")
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("read authz token: %w", err)
	}
	authenticator, err := jwtauthn.New(jwtauthn.Config{
		JWKSURL:             *authJWKSURL,
		Issuer:              *authIssuer,
		Audience:            []string{*authAudience},
		CAFile:              *authCAFile,
		TokenCacheTTL:       *authTokenCacheTTL,
		TokenCacheEntries:   *authTokenCacheEntries,
		JWKSCacheTTL:        *authJWKSCacheTTL,
		JWKSRefreshInterval: *authJWKSRefreshInterval,
		HTTPTimeout:         *authHTTPTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure JWT authentication: %w", err)
	}
	defer authenticator.Close()

	upstream := serveHandler(serveConfig{
		MaxBody:     *maxBody,
		UpstreamURL: *upstreamURL,
		Client:      &http.Client{Timeout: *requestTimeout},
	})
	handler := authProxyHandler(authProxyConfig{
		Namespace:     *namespace,
		DomainSuffix:  *domainSuffix,
		Service:       *service,
		Credentials:   authn.NewHeaderCredentials("Bearer " + strings.TrimSpace(string(token))),
		Authenticator: authenticator,
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
		principal, err := config.Authenticator.Authenticate(r.Context(), config.Credentials)
		if err != nil {
			status := http.StatusUnauthorized
			var authnErr *authn.Error
			if errors.As(err, &authnErr) && authnErr.Kind == authn.AuthenticationUnavailable {
				status = http.StatusServiceUnavailable
			}
			if config.LogDenials {
				log.Printf("direct authentication failed: %v", err)
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		req := &authz.Request{
			Principal: principal,
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

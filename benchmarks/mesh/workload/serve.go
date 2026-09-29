package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":8000", "HTTP listen address")
	delay := flags.Duration("delay", 0, "fixed delay before each response")
	maxBody := flags.Int64("max-body-bytes", 1<<20, "largest accepted request body")
	upstreamURL := flags.String("upstream-url", "", "optional URL to forward work through")
	upstreamHost := flags.String("upstream-host", "", "optional Host header for forwarded work")
	requestTimeout := flags.Duration("request-timeout", 10*time.Second, "forwarded request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *maxBody < 1 {
		return fmt.Errorf("max-body-bytes must be positive")
	}
	if *requestTimeout <= 0 {
		return fmt.Errorf("request-timeout must be positive")
	}

	srv := &http.Server{
		Addr: *addr,
		Handler: serveHandler(serveConfig{
			Delay:        *delay,
			MaxBody:      *maxBody,
			UpstreamURL:  *upstreamURL,
			UpstreamHost: *upstreamHost,
			Client:       &http.Client{Timeout: *requestTimeout},
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("serving benchmark workload on %s with delay %s", *addr, *delay)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

type serveConfig struct {
	Delay        time.Duration
	MaxBody      int64
	UpstreamURL  string
	UpstreamHost string
	Client       *http.Client
}

func serveHandler(config serveConfig) http.Handler {
	if config.Client == nil {
		config.Client = http.DefaultClient
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /work", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, config.MaxBody+1))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if int64(len(body)) > config.MaxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if config.Delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(config.Delay):
			}
		}
		if config.UpstreamURL != "" {
			forward, err := http.NewRequestWithContext(r.Context(), http.MethodPost, config.UpstreamURL, bytes.NewReader(body))
			if err != nil {
				http.Error(w, "create upstream request", http.StatusBadGateway)
				return
			}
			forward.Header.Set("Content-Type", r.Header.Get("Content-Type"))
			forward.Host = config.UpstreamHost
			response, err := config.Client.Do(forward)
			if err != nil {
				http.Error(w, "upstream request", http.StatusBadGateway)
				return
			}
			upstreamBody, readErr := io.ReadAll(io.LimitReader(response.Body, config.MaxBody+1))
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || int64(len(upstreamBody)) > config.MaxBody {
				http.Error(w, "read upstream response", http.StatusBadGateway)
				return
			}
			if response.StatusCode != http.StatusOK {
				http.Error(w, "upstream response", http.StatusBadGateway)
				return
			}
			body = upstreamBody
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			log.Printf("write response: %v", err)
		}
	})
	return mux
}

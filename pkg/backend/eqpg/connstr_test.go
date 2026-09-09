package eqpg

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func TestBuildConnStrURL(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "preserves URL connection parameters",
			target: "postgresql://aso:p%40ss@database.example:5432/aso?sslmode=require&application_name=worker",
			want:   "postgresql://aso:p%40ss@database.example:5432/aso?application_name=worker&search_path=entroq%2Cpublic&sslmode=require",
		},
		{
			name:   "forces EntroQ search path",
			target: "postgres://aso:aso@postgres:5432/aso?search_path=public&sslmode=disable",
			want:   "postgres://aso:aso@postgres:5432/aso?search_path=entroq%2Cpublic&sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildConnStr(tt.target, defaultOptions(nil))
			if err != nil {
				t.Fatalf("buildConnStr: %v", err)
			}
			if got != tt.want {
				t.Fatalf("buildConnStr = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildConnStrURLRejectsOtherSchemes(t *testing.T) {
	_, err := buildConnStr("mysql://user:secret@database.example/db", defaultOptions(nil))
	if err == nil {
		t.Fatal("buildConnStr accepted a non-PostgreSQL URL")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("buildConnStr error exposed URL credentials: %v", err)
	}
}

func TestBuildConnStrURLParseErrorRedactsCredentials(t *testing.T) {
	_, err := buildConnStr("postgres://user:secret%@database.example/db", defaultOptions(nil))
	if err == nil {
		t.Fatal("buildConnStr accepted a malformed PostgreSQL URL")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("buildConnStr error exposed URL credentials: %v", err)
	}
}

func TestBuildConnStrTLSFiles(t *testing.T) {
	options := defaultOptions([]PGOpt{
		WithSSL(SSLVerifyFull),
		WithSSLClientFiles("/certs/client cert.pem", "/certs/client key.pem"),
		WithSSLServerCAFile("/certs/root cert.pem"),
	})
	connStr, err := buildConnStr("database.example:5432", options)
	if err != nil {
		t.Fatalf("buildConnStr: %v", err)
	}

	cfg, err := pq.NewConfig(connStr)
	if err != nil {
		t.Fatalf("parse built connection string: %v", err)
	}
	if cfg.SSLMode != pq.SSLModeVerifyFull {
		t.Errorf("SSL mode = %q, want verify-full", cfg.SSLMode)
	}
	if cfg.SSLCert != "/certs/client cert.pem" {
		t.Errorf("SSL certificate = %q", cfg.SSLCert)
	}
	if cfg.SSLKey != "/certs/client key.pem" {
		t.Errorf("SSL key = %q", cfg.SSLKey)
	}
	if cfg.SSLRootCert != "/certs/root cert.pem" {
		t.Errorf("SSL root certificate = %q", cfg.SSLRootCert)
	}
}

func TestOpenURL(t *testing.T) {
	target := (&url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword("postgres", "password"),
		Host:     pgHostPort,
		Path:     "postgres",
		RawQuery: "sslmode=disable",
	}).String()

	backend, err := Open(context.Background(), target)
	if err != nil {
		t.Fatalf("Open URL: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close backend: %v", err)
	}
}

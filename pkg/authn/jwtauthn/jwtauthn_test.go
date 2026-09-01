package jwtauthn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shiblon/entroq/pkg/authn"
)

const (
	testIssuer   = "https://issuer.example"
	testAudience = "entroq-test"
)

func testKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func testJWKS(key *rsa.PrivateKey, kid string) []byte {
	public := key.Public().(*rsa.PublicKey)
	document := map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}}}
	data, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return data
}

func testServer(t testing.TB, document []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	requests := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func testConfig(jwksURL string) Config {
	return Config{
		JWKSURL:             jwksURL,
		Issuer:              testIssuer,
		Audience:            []string{testAudience},
		TokenCacheTTL:       time.Minute,
		TokenCacheEntries:   16,
		JWKSCacheTTL:        time.Hour,
		JWKSRefreshInterval: time.Minute,
		HTTPTimeout:         time.Second,
	}
}

func testToken(t testing.TB, key *rsa.PrivateKey, kid, subject, issuer, audience string, expires time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":  subject,
		"iss":  issuer,
		"aud":  audience,
		"exp":  expires.Unix(),
		"iat":  time.Now().Add(-time.Second).Unix(),
		"role": "gateway",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func TestAuthenticateCachesVerifiedPrincipal(t *testing.T) {
	key := testKey(t)
	server, requests := testServer(t, testJWKS(key, "key-1"))
	authenticator, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	raw := testToken(t, key, "key-1", "service-a", testIssuer, testAudience, time.Now().Add(time.Hour))
	credentials := &authn.Credentials{Scheme: "bearer", Token: raw}
	first, err := authenticator.Authenticate(context.Background(), credentials)
	if err != nil {
		t.Fatalf("first Authenticate: %v", err)
	}
	if first.Subject != "service-a" || first.Issuer != testIssuer || first.ExpiresAt == 0 {
		t.Fatalf("principal = %#v", first)
	}
	if string(first.Claims) == "" || !strings.Contains(string(first.Claims), `"role":"gateway"`) {
		t.Fatalf("verified claims = %s, want custom role", first.Claims)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), raw) {
		t.Fatal("principal JSON contains the bearer token")
	}

	// Mutating the caller's copy must not alter the cached principal.
	first.Subject = "mutated"
	first.Audience[0] = "mutated"
	first.Claims[0] = '['
	second, err := authenticator.Authenticate(context.Background(), credentials)
	if err != nil {
		t.Fatalf("cached Authenticate: %v", err)
	}
	if second.Subject != "service-a" || second.Audience[0] != testAudience || second.Claims[0] != '{' {
		t.Fatalf("cached principal was mutated: %#v claims=%s", second, second.Claims)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1", got)
	}
}

func TestAuthenticateFromJWKSFile(t *testing.T) {
	key := testKey(t)
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, testJWKS(key, "key-1"), 0o600); err != nil {
		t.Fatalf("write JWKS: %v", err)
	}
	config := testConfig("")
	config.JWKSFile = path
	authenticator, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	raw := testToken(t, key, "key-1", "service-a", testIssuer, testAudience, time.Now().Add(time.Hour))
	principal, err := authenticator.Authenticate(context.Background(), &authn.Credentials{Scheme: "Bearer", Token: raw})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Subject != "service-a" {
		t.Fatalf("principal subject = %q, want service-a", principal.Subject)
	}
}

func TestAuthenticateRejectsInvalidCredentials(t *testing.T) {
	key := testKey(t)
	server, _ := testServer(t, testJWKS(key, "key-1"))
	authenticator, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	validExpiry := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name        string
		credentials *authn.Credentials
	}{
		{name: "missing"},
		{name: "wrong scheme", credentials: &authn.Credentials{Scheme: "Basic", Token: "value"}},
		{name: "expired", credentials: &authn.Credentials{Scheme: "Bearer", Token: testToken(t, key, "key-1", "service-a", testIssuer, testAudience, time.Now().Add(-time.Minute))}},
		{name: "wrong issuer", credentials: &authn.Credentials{Scheme: "Bearer", Token: testToken(t, key, "key-1", "service-a", "wrong", testAudience, validExpiry)}},
		{name: "wrong audience", credentials: &authn.Credentials{Scheme: "Bearer", Token: testToken(t, key, "key-1", "service-a", testIssuer, "wrong", validExpiry)}},
		{name: "missing subject", credentials: &authn.Credentials{Scheme: "Bearer", Token: testToken(t, key, "key-1", "", testIssuer, testAudience, validExpiry)}},
		{name: "unknown key", credentials: &authn.Credentials{Scheme: "Bearer", Token: testToken(t, key, "unknown", "service-a", testIssuer, testAudience, validExpiry)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := authenticator.Authenticate(context.Background(), tc.credentials)
			var authErr *authn.Error
			if !errors.As(err, &authErr) || authErr.Kind != authn.InvalidCredentials {
				t.Fatalf("Authenticate error = %v, want invalid credentials", err)
			}
		})
	}
}

func TestAuthenticateRejectsSymmetricAlgorithm(t *testing.T) {
	key := testKey(t)
	server, _ := testServer(t, testJWKS(key, "key-1"))
	authenticator, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "service-a",
		"iss": testIssuer,
		"aud": testAudience,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "key-1"
	raw, err := token.SignedString([]byte("attacker-controlled-secret"))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	_, err = authenticator.Authenticate(context.Background(), &authn.Credentials{Scheme: "Bearer", Token: raw})
	var authErr *authn.Error
	if !errors.As(err, &authErr) || authErr.Kind != authn.InvalidCredentials {
		t.Fatalf("Authenticate error = %v, want invalid credentials", err)
	}
}

func TestNewRejectsUnsafeJWKSURL(t *testing.T) {
	for _, jwksURL := range []string{
		"http://issuer.example/jwks",
		"https://user:password@issuer.example/jwks",
		"file:///tmp/jwks.json",
	} {
		t.Run(jwksURL, func(t *testing.T) {
			_, err := New(testConfig(jwksURL))
			if err == nil {
				t.Fatal("New succeeded with unsafe JWKS URL")
			}
		})
	}
}

func TestJWKSRedirectCannotDowngradeHTTPS(t *testing.T) {
	config := testConfig("https://issuer.example/jwks")
	authenticator, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	request := httptest.NewRequest(http.MethodGet, "http://issuer.example/jwks", nil)
	err = authenticator.client.CheckRedirect(request, []*http.Request{{}})
	if err == nil {
		t.Fatal("CheckRedirect allowed HTTPS downgrade to remote HTTP")
	}
}

func TestAuthenticateClassifiesJWKSFailureAsUnavailable(t *testing.T) {
	key := testKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	server.Close()
	authenticator, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	raw := testToken(t, key, "key-1", "service-a", testIssuer, testAudience, time.Now().Add(time.Hour))
	_, err = authenticator.Authenticate(context.Background(), &authn.Credentials{Scheme: "Bearer", Token: raw})
	var authErr *authn.Error
	if !errors.As(err, &authErr) || authErr.Kind != authn.AuthenticationUnavailable {
		t.Fatalf("Authenticate error = %v, want authentication unavailable", err)
	}
}

func TestTokenCacheIsBounded(t *testing.T) {
	key := testKey(t)
	server, _ := testServer(t, testJWKS(key, "key-1"))
	config := testConfig(server.URL)
	config.TokenCacheEntries = 2
	authenticator, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	for _, subject := range []string{"one", "two", "three"} {
		raw := testToken(t, key, "key-1", subject, testIssuer, testAudience, time.Now().Add(time.Hour))
		if _, err := authenticator.Authenticate(context.Background(), &authn.Credentials{Scheme: "Bearer", Token: raw}); err != nil {
			t.Fatalf("Authenticate %q: %v", subject, err)
		}
	}
	if got := len(authenticator.cache); got != 2 {
		t.Fatalf("token cache entries = %d, want 2", got)
	}
}

func TestUnknownKIDRefreshIsRateLimited(t *testing.T) {
	key := testKey(t)
	server, requests := testServer(t, testJWKS(key, "key-1"))
	config := testConfig(server.URL)
	config.JWKSRefreshInterval = time.Hour
	authenticator, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	valid := testToken(t, key, "key-1", "valid", testIssuer, testAudience, time.Now().Add(time.Hour))
	if _, err := authenticator.Authenticate(context.Background(), &authn.Credentials{Scheme: "Bearer", Token: valid}); err != nil {
		t.Fatalf("Authenticate valid token: %v", err)
	}
	unknown := testToken(t, key, "attacker-selected", "unknown", testIssuer, testAudience, time.Now().Add(time.Hour))
	for range 3 {
		_, _ = authenticator.Authenticate(context.Background(), &authn.Credentials{Scheme: "Bearer", Token: unknown})
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1 despite unknown kids", got)
	}
}

func BenchmarkAuthenticate(b *testing.B) {
	key := testKey(b)
	server, _ := testServer(b, testJWKS(key, "key-1"))
	raw := testToken(b, key, "key-1", "service-a", testIssuer, testAudience, time.Now().Add(time.Hour))
	credentials := &authn.Credentials{Scheme: "Bearer", Token: raw}

	for _, tc := range []struct {
		name     string
		cacheTTL time.Duration
	}{
		{name: "cached", cacheTTL: time.Minute},
		{name: "verify-every-call"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			config := testConfig(server.URL)
			config.TokenCacheTTL = tc.cacheTTL
			authenticator, err := New(config)
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			b.Cleanup(func() { _ = authenticator.Close() })
			if _, err := authenticator.Authenticate(context.Background(), credentials); err != nil {
				b.Fatalf("warm Authenticate: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := authenticator.Authenticate(context.Background(), credentials); err != nil {
					b.Fatalf("Authenticate: %v", err)
				}
			}
		})
	}
}

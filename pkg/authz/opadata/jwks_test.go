package authz

// jwks_test.go exercises the full policy stack -- including user.rego JWT
// verification -- using a local mock JWKS server backed by a generated RSA key.
// No external IDP is required.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage/inmem"
)

// rsaKey is the test key pair, generated once for the package.
var rsaKey *rsa.PrivateKey

func init() {
	var err error
	rsaKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("jwks_test: generate RSA key: %v", err)
	}
}

// jwksServer starts an httptest.Server that serves the public RSA key as a
// minimal JWKS document. The caller must close the server.
func jwksServer(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	pub := key.Public().(*rsa.PublicKey)
	doc := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": "test-key",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// makeToken signs a JWT for the given subject using the test RSA key.
func makeToken(t *testing.T, key *rsa.PrivateKey, subject, audience, issuer string, expiry time.Duration) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		Subject:   subject,
		Audience:  jwt.ClaimStrings{audience},
		Issuer:    issuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// evalAllow loads all Rego modules from the embedded conf/ FS and evaluates
// data.entroq.authz.allow with the given input and store data.
func evalQuery(t *testing.T, query string, input map[string]any, storeData map[string]any) rego.ResultSet {
	t.Helper()
	ctx := context.Background()

	mods, err := parseModules()
	if err != nil {
		t.Fatalf("parse modules: %v", err)
	}

	store := inmem.NewFromObject(storeData)

	var regoOpts []func(*rego.Rego)
	regoOpts = append(regoOpts,
		rego.Query(query),
		rego.Input(input),
		rego.Store(store),
	)
	for _, mod := range mods {
		regoOpts = append(regoOpts, rego.ParsedModule(mod))
	}

	rs, err := rego.New(regoOpts...).Eval(ctx)
	if err != nil {
		t.Fatalf("eval %q: %v", query, err)
	}
	return rs
}

func evalAllow(t *testing.T, input map[string]any, storeData map[string]any) bool {
	t.Helper()
	rs := evalQuery(t, "data.entroq.authz.allow", input, storeData)
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return false
	}
	v, _ := rs[0].Expressions[0].Value.(bool)
	return v
}

func TestJWKSAllow(t *testing.T) {
	const (
		audience = "test-audience"
		issuer   = "test-issuer"
		username = "auser"
	)

	srv := jwksServer(t, rsaKey)

	// Store data: IDP config + policy users/roles.
	storeData := map[string]any{
		"entroq": map[string]any{
			"idp": map[string]any{
				"jwks_url": srv.URL,
				"audience": audience,
				"issuer":   issuer,
			},
			"policy": map[string]any{
				"users": []any{
					map[string]any{
						"name":  username,
						"roles": []any{},
						"queues": []any{
							map[string]any{
								"exact":   "/shared/inbox",
								"actions": []any{"CLAIM", "DELETE"},
							},
						},
					},
				},
				"roles": []any{},
			},
		},
	}

	t.Run("valid token, allowed queue and action", func(t *testing.T) {
		tok := makeToken(t, rsaKey, username, audience, issuer, time.Hour)
		input := map[string]any{
			"authz": map[string]any{
				"type":        "Bearer",
				"credentials": tok,
			},
			"queues": []any{
				map[string]any{
					"exact":   "/shared/inbox",
					"actions": []any{"CLAIM"},
				},
			},
		}
		if !evalAllow(t, input, storeData) {
			t.Error("expected allow=true, got false")
		}
	})

	t.Run("valid token, disallowed action", func(t *testing.T) {
		tok := makeToken(t, rsaKey, username, audience, issuer, time.Hour)
		input := map[string]any{
			"authz": map[string]any{
				"type":        "Bearer",
				"credentials": tok,
			},
			"queues": []any{
				map[string]any{
					// INSERT is not in the explicit policy for /shared/inbox.
					"exact":   "/shared/inbox",
					"actions": []any{"INSERT"},
				},
			},
		}
		if evalAllow(t, input, storeData) {
			t.Error("expected allow=false, got true")
		}
	})

	t.Run("valid token, wrong queue", func(t *testing.T) {
		tok := makeToken(t, rsaKey, username, audience, issuer, time.Hour)
		input := map[string]any{
			"authz": map[string]any{
				"type":        "Bearer",
				"credentials": tok,
			},
			"queues": []any{
				map[string]any{
					// /shared/secret is not in any policy.
					"exact":   "/shared/secret",
					"actions": []any{"CLAIM"},
				},
			},
		}
		if evalAllow(t, input, storeData) {
			t.Error("expected allow=false, got true")
		}
	})

	t.Run("expired token", func(t *testing.T) {
		tok := makeToken(t, rsaKey, username, audience, issuer, -time.Minute)
		input := map[string]any{
			"authz": map[string]any{
				"type":        "Bearer",
				"credentials": tok,
			},
			"queues": []any{
				map[string]any{
					"exact":   "/shared/inbox",
					"actions": []any{"CLAIM"},
				},
			},
		}
		if evalAllow(t, input, storeData) {
			t.Error("expected allow=false for expired token, got true")
		}
	})

	t.Run("wrong audience", func(t *testing.T) {
		tok := makeToken(t, rsaKey, username, "wrong-audience", issuer, time.Hour)
		input := map[string]any{
			"authz": map[string]any{
				"type":        "Bearer",
				"credentials": tok,
			},
			"queues": []any{
				map[string]any{
					"exact":   "/shared/inbox",
					"actions": []any{"CLAIM"},
				},
			},
		}
		if evalAllow(t, input, storeData) {
			t.Error("expected allow=false for wrong audience, got true")
		}
	})

	t.Run("personal namespace prefix allowed", func(t *testing.T) {
		tok := makeToken(t, rsaKey, username, audience, issuer, time.Hour)
		input := map[string]any{
			"authz": map[string]any{
				"type":        "Bearer",
				"credentials": tok,
			},
			"queues": []any{
				map[string]any{
					// /users/<username>/ prefix is auto-granted to everyone.
					"exact":   "/users/auser/anything",
					"actions": []any{"CLAIM", "INSERT", "DELETE"},
				},
			},
		}
		if !evalAllow(t, input, storeData) {
			t.Error("expected allow=true for personal namespace, got false")
		}
	})
}

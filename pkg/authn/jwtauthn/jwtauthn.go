// Package jwtauthn authenticates bearer JWTs against a JSON Web Key Set.
package jwtauthn

import (
	"container/list"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shiblon/entroq/pkg/authn"
	"golang.org/x/sync/singleflight"
)

const maxJWKSSize = 4 << 20

var validMethods = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512",
	"EdDSA",
}

// Config configures JWT verification and its bounded caches. Exactly one of
// JWKSURL and JWKSFile must be set.
type Config struct {
	// JWKSURL is the HTTP(S) key-set endpoint. Non-loopback endpoints must use HTTPS.
	JWKSURL string
	// JWKSFile is a local key-set file, mutually exclusive with JWKSURL.
	JWKSFile string
	// Issuer is the required JWT issuer.
	Issuer string
	// Audience lists acceptable JWT audiences.
	Audience []string
	// CAFile contains additional PEM certificates for JWKS HTTPS.
	CAFile string
	// TokenCacheTTL bounds successful verification caching; zero disables it.
	TokenCacheTTL time.Duration
	// TokenCacheEntries bounds the verified-token LRU.
	TokenCacheEntries int
	// JWKSCacheTTL controls key-set freshness.
	JWKSCacheTTL time.Duration
	// JWKSRefreshInterval rate-limits refreshes caused by unknown key IDs.
	JWKSRefreshInterval time.Duration
	// HTTPTimeout bounds JWKS requests.
	HTTPTimeout time.Duration
}

// Authenticator verifies JWT bearer credentials. It caches successful
// verification by token digest, never by subject, and never caches an
// authorization decision.
type Authenticator struct {
	config Config
	client *http.Client

	cacheMu sync.Mutex
	cache   map[[sha256.Size]byte]*list.Element
	lru     list.List

	keysMu     sync.RWMutex
	keys       []verificationKey
	keysExpire time.Time
	lastFetch  time.Time
	refresh    singleflight.Group
}

type cacheEntry struct {
	digest    [sha256.Size]byte
	principal authn.VerifiedPrincipal
	until     time.Time
}

type verificationKey struct {
	kid string
	alg string
	key crypto.PublicKey
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// New creates a JWT authenticator. Key material is loaded lazily on the first
// authentication attempt so a transient identity-provider outage does not
// prevent the EntroQ process from starting.
func New(config Config) (*Authenticator, error) {
	if (config.JWKSURL == "") == (config.JWKSFile == "") {
		return nil, fmt.Errorf("jwtauthn: exactly one of JWKSURL and JWKSFile is required")
	}
	if config.Issuer == "" {
		return nil, fmt.Errorf("jwtauthn: issuer is required")
	}
	if len(config.Audience) == 0 {
		return nil, fmt.Errorf("jwtauthn: at least one audience is required")
	}
	for _, audience := range config.Audience {
		if audience == "" {
			return nil, fmt.Errorf("jwtauthn: audience must not be empty")
		}
	}
	if config.TokenCacheTTL < 0 {
		return nil, fmt.Errorf("jwtauthn: token cache TTL must not be negative")
	}
	if config.TokenCacheEntries < 0 {
		return nil, fmt.Errorf("jwtauthn: token cache entries must not be negative")
	}
	if config.TokenCacheTTL > 0 && config.TokenCacheEntries <= 0 {
		return nil, fmt.Errorf("jwtauthn: token cache entries must be positive when caching is enabled")
	}
	if config.JWKSCacheTTL <= 0 {
		return nil, fmt.Errorf("jwtauthn: JWKS cache TTL must be positive")
	}
	if config.JWKSRefreshInterval < 0 {
		return nil, fmt.Errorf("jwtauthn: JWKS refresh interval must not be negative")
	}
	if config.HTTPTimeout <= 0 {
		return nil, fmt.Errorf("jwtauthn: HTTP timeout must be positive")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if config.CAFile != "" {
		pem, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("jwtauthn: read CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("jwtauthn: system CA pool: %w", err)
		}
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("jwtauthn: CA file contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	if config.JWKSURL != "" {
		u, err := url.Parse(config.JWKSURL)
		if err != nil {
			return nil, fmt.Errorf("jwtauthn: invalid JWKS URL")
		}
		if err := validateJWKSURL(u); err != nil {
			return nil, fmt.Errorf("jwtauthn: invalid JWKS URL: %w", err)
		}
	}

	return &Authenticator{
		config: config,
		client: &http.Client{
			Transport: transport,
			Timeout:   config.HTTPTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return validateJWKSURL(req.URL)
			},
		},
		cache: make(map[[sha256.Size]byte]*list.Element),
	}, nil
}

func validateJWKSURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("URL must use HTTP or HTTPS and include a host")
	}
	if u.User != nil {
		return fmt.Errorf("URL must not contain user information")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("non-loopback URL must use HTTPS")
		}
	}
	return nil
}

// Authenticate verifies a Bearer JWT and returns only verified identity facts.
func (a *Authenticator) Authenticate(ctx context.Context, credentials *authn.Credentials) (*authn.VerifiedPrincipal, error) {
	if credentials == nil || !strings.EqualFold(credentials.Scheme, "Bearer") || credentials.Token == "" {
		return nil, authn.InvalidError("Bearer credentials are required", nil)
	}

	digest := sha256.Sum256([]byte(credentials.Token))
	if principal := a.cached(digest, time.Now()); principal != nil {
		return principal, nil
	}

	principal, err := a.verify(ctx, credentials.Token)
	if err != nil {
		return nil, err
	}
	a.cachePrincipal(digest, principal, time.Now())
	return clonePrincipal(principal), nil
}

func (a *Authenticator) verify(ctx context.Context, raw string) (*authn.VerifiedPrincipal, error) {
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		return a.verificationKey(ctx, kid, token.Method.Alg())
	},
		jwt.WithAudience(a.config.Audience...),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithIssuer(a.config.Issuer),
		jwt.WithJSONNumber(),
		jwt.WithValidMethods(validMethods),
	)
	if err != nil {
		var authErr *authn.Error
		if errors.As(err, &authErr) && authErr.Kind == authn.AuthenticationUnavailable {
			return nil, authErr
		}
		return nil, authn.InvalidError("JWT verification failed", err)
	}
	if !token.Valid {
		return nil, authn.InvalidError("JWT verification failed", nil)
	}

	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		return nil, authn.InvalidError("JWT subject is required", err)
	}
	issuer, err := claims.GetIssuer()
	if err != nil {
		return nil, authn.InvalidError("JWT issuer is invalid", err)
	}
	audience, err := claims.GetAudience()
	if err != nil {
		return nil, authn.InvalidError("JWT audience is invalid", err)
	}
	expires, err := claims.GetExpirationTime()
	if err != nil || expires == nil {
		return nil, authn.InvalidError("JWT expiration is required", err)
	}
	encodedClaims, err := json.Marshal(claims)
	if err != nil {
		return nil, authn.InvalidError("JWT claims are not JSON-compatible", err)
	}

	return &authn.VerifiedPrincipal{
		Subject:   subject,
		Issuer:    issuer,
		Audience:  append([]string(nil), audience...),
		ExpiresAt: expires.Unix(),
		Claims:    encodedClaims,
	}, nil
}

func (a *Authenticator) cached(digest [sha256.Size]byte, now time.Time) *authn.VerifiedPrincipal {
	if a.config.TokenCacheTTL == 0 {
		return nil
	}
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	element := a.cache[digest]
	if element == nil {
		return nil
	}
	entry := element.Value.(*cacheEntry)
	if !now.Before(entry.until) {
		a.removeCacheElement(element)
		return nil
	}
	a.lru.MoveToFront(element)
	return clonePrincipal(&entry.principal)
}

func (a *Authenticator) cachePrincipal(digest [sha256.Size]byte, principal *authn.VerifiedPrincipal, now time.Time) {
	if a.config.TokenCacheTTL == 0 {
		return
	}
	until := now.Add(a.config.TokenCacheTTL)
	if expires := time.Unix(principal.ExpiresAt, 0); expires.Before(until) {
		until = expires
	}
	if !now.Before(until) {
		return
	}

	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if element := a.cache[digest]; element != nil {
		entry := element.Value.(*cacheEntry)
		entry.principal = *clonePrincipal(principal)
		entry.until = until
		a.lru.MoveToFront(element)
		return
	}
	entry := &cacheEntry{digest: digest, principal: *clonePrincipal(principal), until: until}
	element := a.lru.PushFront(entry)
	a.cache[digest] = element
	for a.lru.Len() > a.config.TokenCacheEntries {
		a.removeCacheElement(a.lru.Back())
	}
}

func (a *Authenticator) removeCacheElement(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(a.cache, entry.digest)
	a.lru.Remove(element)
}

func clonePrincipal(principal *authn.VerifiedPrincipal) *authn.VerifiedPrincipal {
	if principal == nil {
		return nil
	}
	result := *principal
	result.Audience = append([]string(nil), principal.Audience...)
	result.Claims = append(json.RawMessage(nil), principal.Claims...)
	return &result
}

func (a *Authenticator) verificationKey(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	if !a.keysFresh(time.Now()) {
		if err := a.refreshKeys(ctx, false); err != nil {
			return nil, err
		}
	}
	if key, ok := a.selectKey(kid, alg); ok {
		return key, nil
	}

	a.keysMu.RLock()
	refreshAllowed := time.Since(a.lastFetch) >= a.config.JWKSRefreshInterval
	a.keysMu.RUnlock()
	if refreshAllowed {
		if err := a.refreshKeys(ctx, true); err != nil {
			return nil, err
		}
		if key, ok := a.selectKey(kid, alg); ok {
			return key, nil
		}
	}
	return nil, authn.InvalidError("JWT signing key is unknown", nil)
}

func (a *Authenticator) keysFresh(now time.Time) bool {
	a.keysMu.RLock()
	defer a.keysMu.RUnlock()
	return len(a.keys) > 0 && now.Before(a.keysExpire)
}

func (a *Authenticator) selectKey(kid, alg string) (crypto.PublicKey, bool) {
	a.keysMu.RLock()
	defer a.keysMu.RUnlock()
	var matches []crypto.PublicKey
	for _, key := range a.keys {
		if kid != "" && key.kid != kid {
			continue
		}
		if key.alg != "" && key.alg != alg {
			continue
		}
		if !keySupportsAlgorithm(key.key, alg) {
			continue
		}
		matches = append(matches, key.key)
	}
	if len(matches) != 1 {
		return nil, false
	}
	return matches[0], true
}

func (a *Authenticator) refreshKeys(ctx context.Context, force bool) error {
	result := a.refresh.DoChan("jwks", func() (any, error) {
		if !force && a.keysFresh(time.Now()) {
			return nil, nil
		}
		keys, err := a.fetchKeys()
		if err != nil {
			return nil, err
		}
		now := time.Now()
		a.keysMu.Lock()
		a.keys = keys
		a.lastFetch = now
		a.keysExpire = now.Add(a.config.JWKSCacheTTL)
		a.keysMu.Unlock()
		return nil, nil
	})
	select {
	case <-ctx.Done():
		return authn.UnavailableError("JWT key refresh was canceled", ctx.Err())
	case value := <-result:
		if value.Err != nil {
			return authn.UnavailableError("JWT signing keys are unavailable", value.Err)
		}
		return nil
	}
}

func (a *Authenticator) fetchKeys() ([]verificationKey, error) {
	var data []byte
	var err error
	if a.config.JWKSFile != "" {
		data, err = os.ReadFile(a.config.JWKSFile)
		if err != nil {
			return nil, fmt.Errorf("read JWKS file: %w", err)
		}
		if len(data) > maxJWKSSize {
			return nil, fmt.Errorf("JWKS file exceeds %d bytes", maxJWKSSize)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), a.config.HTTPTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.config.JWKSURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create JWKS request: %w", err)
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch JWKS: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch JWKS: HTTP status %d", resp.StatusCode)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxJWKSSize+1))
		if err != nil {
			return nil, fmt.Errorf("read JWKS: %w", err)
		}
		if len(data) > maxJWKSSize {
			return nil, fmt.Errorf("JWKS response exceeds %d bytes", maxJWKSSize)
		}
	}

	var document jwksDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	var keys []verificationKey
	for _, raw := range document.Keys {
		if raw.Use != "" && raw.Use != "sig" {
			continue
		}
		if raw.Kty != "RSA" && raw.Kty != "EC" && raw.Kty != "OKP" {
			continue
		}
		key, err := parseJWK(raw)
		if err != nil {
			return nil, fmt.Errorf("decode JWK %q: %w", raw.Kid, err)
		}
		keys = append(keys, verificationKey{kid: raw.Kid, alg: raw.Alg, key: key})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no signature keys")
	}
	return keys, nil
}

func parseJWK(raw jwk) (crypto.PublicKey, error) {
	switch raw.Kty {
	case "RSA":
		n, err := decodeBase64URL(raw.N)
		if err != nil {
			return nil, fmt.Errorf("decode RSA modulus: %w", err)
		}
		e, err := decodeBase64URL(raw.E)
		if err != nil {
			return nil, fmt.Errorf("decode RSA exponent: %w", err)
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA modulus is smaller than 2048 bits")
		}
		if exponent < 3 || exponent%2 == 0 {
			return nil, fmt.Errorf("RSA exponent is invalid")
		}
		return &rsa.PublicKey{N: modulus, E: exponent}, nil
	case "EC":
		var curve elliptic.Curve
		switch raw.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported EC curve %q", raw.Crv)
		}
		x, err := decodeBase64URL(raw.X)
		if err != nil {
			return nil, fmt.Errorf("decode EC x coordinate: %w", err)
		}
		y, err := decodeBase64URL(raw.Y)
		if err != nil {
			return nil, fmt.Errorf("decode EC y coordinate: %w", err)
		}
		public := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(public.X, public.Y) {
			return nil, fmt.Errorf("EC point is not on curve")
		}
		return public, nil
	case "OKP":
		if raw.Crv != "Ed25519" {
			return nil, fmt.Errorf("unsupported OKP curve %q", raw.Crv)
		}
		x, err := decodeBase64URL(raw.X)
		if err != nil {
			return nil, fmt.Errorf("decode Ed25519 key: %w", err)
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("Ed25519 key has length %d", len(x))
		}
		return ed25519.PublicKey(x), nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", raw.Kty)
	}
}

func decodeBase64URL(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("value is empty")
	}
	return base64.RawURLEncoding.DecodeString(value)
}

func keySupportsAlgorithm(key crypto.PublicKey, alg string) bool {
	switch key.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	case *ecdsa.PublicKey:
		switch alg {
		case "ES256":
			return key.(*ecdsa.PublicKey).Curve == elliptic.P256()
		case "ES384":
			return key.(*ecdsa.PublicKey).Curve == elliptic.P384()
		case "ES512":
			return key.(*ecdsa.PublicKey).Curve == elliptic.P521()
		default:
			return false
		}
	case ed25519.PublicKey:
		return alg == "EdDSA"
	default:
		return false
	}
}

// Close releases idle HTTP connections held by the authenticator.
func (a *Authenticator) Close() error {
	if a != nil && a.client != nil {
		a.client.CloseIdleConnections()
	}
	return nil
}

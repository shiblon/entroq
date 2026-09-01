// Package authn defines the authentication boundary used by EntroQ services.
package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Authenticator turns untrusted request credentials into verified identity
// facts. Implementations must not return a VerifiedPrincipal until every credential
// invariant they claim to enforce has been checked.
type Authenticator interface {
	Authenticate(context.Context, *Credentials) (*VerifiedPrincipal, error)
	Close() error
}

// Credentials are parsed from an incoming Authorization header. They remain
// outside authorization inputs so a policy engine never receives bearer
// secrets after authentication succeeds.
type Credentials struct {
	// Scheme is the Authorization scheme, such as Bearer.
	Scheme string
	// Token is the untrusted credential value. It must never be copied into an
	// authorization request or diagnostic.
	Token string
}

// NewHeaderCredentials parses an Authorization header into its scheme and
// credential value. Validation of the scheme and credential belongs to the
// selected Authenticator.
func NewHeaderCredentials(value string) *Credentials {
	value = strings.TrimSpace(value)
	if value == "" {
		return new(Credentials)
	}
	for i, r := range value {
		if r == ' ' || r == '\t' {
			return &Credentials{
				Scheme: value[:i],
				Token:  strings.TrimSpace(value[i+1:]),
			}
		}
	}
	return &Credentials{Scheme: value}
}

// VerifiedPrincipal contains facts established by an Authenticator. Claims is the
// verified JWT payload as JSON so environment policy can inspect custom claims
// without receiving or decoding the bearer token itself.
type VerifiedPrincipal struct {
	// Subject is the authenticated identity asserted by the issuer.
	Subject string `json:"subject"`
	// Issuer is the verified JWT issuer.
	Issuer string `json:"issuer"`
	// Audience contains the verified JWT audiences.
	Audience []string `json:"audience"`
	// ExpiresAt is the verified JWT expiration as Unix seconds.
	ExpiresAt int64 `json:"expires_at"`
	// Claims is the complete verified JWT payload encoded as JSON.
	Claims json.RawMessage `json:"claims"`
}

// ErrorKind classifies an authentication failure without exposing credentials.
type ErrorKind uint8

const (
	// InvalidCredentials means the caller did not present acceptable credentials.
	InvalidCredentials ErrorKind = iota + 1
	// AuthenticationUnavailable means credentials could not be checked because
	// authentication infrastructure was unavailable.
	AuthenticationUnavailable
)

// Error is a structured authentication failure. Reason must never contain a
// raw credential; it may be returned to callers or written to logs.
type Error struct {
	// Kind distinguishes bad credentials from unavailable authentication
	// infrastructure.
	Kind ErrorKind
	// Reason is credential-safe text suitable for a client response.
	Reason string
	// Err is the underlying error retained for structured inspection.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil || e.Reason == "" {
		return "authentication failed"
	}
	return fmt.Sprintf("authentication failed: %s", e.Reason)
}

// Unwrap returns the underlying infrastructure or validation error, if any.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// InvalidError creates an invalid-credentials error with a credential-safe
// reason.
func InvalidError(reason string, err error) *Error {
	return &Error{Kind: InvalidCredentials, Reason: reason, Err: err}
}

// UnavailableError creates an authentication-infrastructure error with a
// credential-safe reason.
func UnavailableError(reason string, err error) *Error {
	return &Error{Kind: AuthenticationUnavailable, Reason: reason, Err: err}
}

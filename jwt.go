package entroq

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// ClaimantSeparator separates the sub and nonce parts of a composite claimant ID.
const ClaimantSeparator = "#"

// MustParseJWTSub extracts the "sub" (subject) claim from a signed JWT (JWS)
// and returns it as a string. Calls log.Fatalf if the token is malformed or
// has no sub claim.
//
// Note: this only works with signed JWTs (JWS). Encrypted tokens (JWE) have
// an opaque payload and are not supported.
func MustParseJWTSub(token string) string {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 {
		log.Fatalf("MustParseJWTSub: not a JWT: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		log.Fatalf("MustParseJWTSub: decode payload: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		log.Fatalf("MustParseJWTSub: unmarshal claims: %v", err)
	}
	if claims.Sub == "" {
		log.Fatalf("MustParseJWTSub: token has no sub claim: %q", fmt.Sprint(token[:min(len(token), 20)]))
	}
	return claims.Sub
}

// MustClaimantFromSub constructs a composite claimant ID from a signed JWT
// (JWS). The result has the form "<sub>#<nonce>", where sub is the JWT subject
// claim and nonce is a freshly generated random ID. This binds the claimant to
// the authenticated identity while preserving per-process uniqueness.
//
// Use at startup to create a claimant ID for an authenticated worker:
//
//	eq, err := entroq.New(ctx, opener, entroq.WithClaimantID(entroq.MustClaimantFromSub(myToken)))
//
// When the server runs an OPA policy that checks that input.claimant_id starts
// with the token subject (user.name + "#"), cross-identity impersonation is
// blocked while within-identity process isolation is maintained by the nonce.
//
// Note: this only works with signed JWTs (JWS). Encrypted tokens (JWE) have
// an opaque payload and are not supported.
func MustClaimantFromSub(token string) string {
	sub := MustParseJWTSub(token)
	return sub + ClaimantSeparator + GenHex16()
}

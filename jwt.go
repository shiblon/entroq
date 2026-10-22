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
// and returns it as a string. Calls log.Panicf if the token is malformed or
// has no sub claim.
//
// Note: this only works with signed JWTs (JWS). Encrypted tokens (JWE) have
// an opaque payload and are not supported.
func MustParseJWTSub(token string) string {
	sub, err := parseJWTSub(token)
	if err != nil {
		log.Panicf("MustParseJWTSub: %v", err)
	}
	return sub
}

// parseJWTSub reads the "sub" claim of a signed JWT. It decodes the token and
// verifies nothing: the server checks the signature, and binds claimants to
// the subject it verified.
func parseJWTSub(token string) (string, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 {
		return "", InvalidArgumentf("not a JWT: %q", token[:min(len(token), 20)])
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", InvalidArgumentf("decode JWT payload: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", InvalidArgumentf("unmarshal JWT claims: %v", err)
	}
	if claims.Sub == "" {
		return "", InvalidArgumentf("JWT has no sub claim: %q", fmt.Sprint(token[:min(len(token), 20)]))
	}
	return claims.Sub, nil
}

// MustClaimantFromSub constructs a composite claimant ID from a signed JWT
// (JWS). The result has the form "<sub>#<nonce>", where sub is the JWT subject
// claim and nonce is a freshly generated random ID. This binds the claimant to
// the authenticated identity while preserving per-process uniqueness.
//
// A client given WithSubjectFromJWT generates claimants of this form itself,
// one for each worker Run, so this is for building one by hand, such as a
// claimant passed to Scoped with DangerousClaimant:
//
//	run := eq.Scoped(entroq.DangerousClaimant(entroq.MustClaimantFromSub(myToken)))
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

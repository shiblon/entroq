package authn

import "testing"

func TestNewHeaderCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		scheme string
		token  string
	}{
		{name: "empty"},
		{name: "bearer", header: "Bearer token", scheme: "Bearer", token: "token"},
		{name: "surrounding whitespace", header: "  Bearer\t token-value  ", scheme: "Bearer", token: "token-value"},
		{name: "scheme only", header: "Bearer", scheme: "Bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NewHeaderCredentials(tc.header)
			if got.Scheme != tc.scheme || got.Token != tc.token {
				t.Fatalf("NewHeaderCredentials(%q) = %#v, want scheme %q token %q", tc.header, got, tc.scheme, tc.token)
			}
		})
	}
}

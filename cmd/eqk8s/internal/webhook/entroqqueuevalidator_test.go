package webhook

import (
	"testing"

	entroqv1alpha1 "github.com/shiblon/entroq/cmd/eqk8s/api/v1alpha1"
)

func TestValidatePatternValid(t *testing.T) {
	cases := []struct {
		pattern   string
		matchType entroqv1alpha1.MatchType
	}{
		{"/foo/bar", entroqv1alpha1.MatchExact},
		{"/foo/bar/baz", entroqv1alpha1.MatchExact},
		{"/", entroqv1alpha1.MatchExact},
		{"/foo/bar/", entroqv1alpha1.MatchPrefix},
		{"/", entroqv1alpha1.MatchPrefix},
		{"/payments/svc-b/inbox", entroqv1alpha1.MatchExact},
		{"/payments/svc-b/", entroqv1alpha1.MatchPrefix},
	}
	for _, c := range cases {
		if err := validatePattern(c.pattern, c.matchType); err != nil {
			t.Errorf("validatePattern(%q, %q): unexpected error: %v", c.pattern, c.matchType, err)
		}
	}
}

func TestValidatePatternNoLeadingSlash(t *testing.T) {
	if err := validatePattern("foo/bar", entroqv1alpha1.MatchExact); err == nil {
		t.Error("expected error for pattern without leading slash")
	}
}

func TestValidatePatternEmptySegment(t *testing.T) {
	if err := validatePattern("/foo//bar", entroqv1alpha1.MatchExact); err == nil {
		t.Error("expected error for pattern with empty segment (//)")
	}
}

func TestValidatePatternTraversal(t *testing.T) {
	if err := validatePattern("/foo/../bar", entroqv1alpha1.MatchExact); err == nil {
		t.Error("expected error for pattern with .. traversal")
	}
}

func TestValidatePatternPrefixRequiresTrailingSlash(t *testing.T) {
	if err := validatePattern("/foo/bar", entroqv1alpha1.MatchPrefix); err == nil {
		t.Error("expected error for Prefix pattern without trailing slash")
	}
}

func TestValidatePatternExactNoTrailingSlash(t *testing.T) {
	if err := validatePattern("/foo/bar/", entroqv1alpha1.MatchExact); err == nil {
		t.Error("expected error for Exact pattern with trailing slash")
	}
}

func TestValidatePatternExactRootSlashAllowed(t *testing.T) {
	if err := validatePattern("/", entroqv1alpha1.MatchExact); err != nil {
		t.Errorf("root / should be valid for Exact, got: %v", err)
	}
}

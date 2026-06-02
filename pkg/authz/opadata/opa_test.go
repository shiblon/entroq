package authz

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"embed"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/tester"
)

var (
	//go:embed conf
	regoFS embed.FS
)

// parseModules loads .rego files from the embedded conf/ FS. keep is called
// with each file path; return true to include it. If keep is nil, all files
// are included.
func parseModules(keep func(string) bool) (map[string]*ast.Module, error) {
	result := make(map[string]*ast.Module)

	if err := fs.WalkDir(regoFS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk: %w", err)
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".rego") {
			return nil
		}
		if keep != nil && !keep(path) {
			return nil
		}
		b, err := regoFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("walk: %w", err)
		}
		m, err := ast.ParseModule(path, string(b))
		if err != nil {
			return fmt.Errorf("walk: %w", err)
		}
		result[path] = m
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	return result, nil
}

// hasPrefix reports whether path begins with any of the given prefixes.
func hasPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// TestRego runs the OPA policy tests for each provider in isolation.
// The two providers (entroq/OIDC and k8s) define rules in the same package and
// must not be loaded together — each is an alternative deployment choice.
func TestRego(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		prefixes []string
	}{
		{
			name: "oidc",
			prefixes: []string{
				"conf/core/",
				"conf/providers/entroq/",
				"conf/tests/core/",
				"conf/tests/providers/entroq/",
			},
		},
		{
			name: "k8s",
			prefixes: []string{
				"conf/core/",
				"conf/providers/k8s/",
				"conf/tests/core/",
				"conf/tests/providers/k8s/",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mods, err := parseModules(func(path string) bool {
				return hasPrefix(path, tc.prefixes)
			})
			if err != nil {
				t.Fatalf("parse modules: %v", err)
			}

			rch, err := tester.NewRunner().
				SetModules(mods).
				CapturePrintOutput(true).
				EnableTracing(true).
				RunTests(ctx, nil)
			if err != nil {
				t.Fatalf("run tests: %v", err)
			}

			// Collect results so we can both display them and check for failures.
			var results []*tester.Result
			for r := range rch {
				results = append(results, r)
			}

			replay := make(chan *tester.Result, len(results))
			for _, r := range results {
				replay <- r
			}
			close(replay)
			(tester.PrettyReporter{Output: os.Stderr, Verbose: true}).Report(replay) //nolint:errcheck

			for _, r := range results {
				if r.Fail {
					t.Errorf("OPA test failed: %s", r.Name)
				}
			}
		})
	}
}

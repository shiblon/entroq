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

func parseModules() (map[string]*ast.Module, error) {
	result := make(map[string]*ast.Module)

	if err := fs.WalkDir(regoFS, ".", func(path string, entry fs.DirEntry, err error) error {
		if !strings.HasSuffix(path, ".rego") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("walk: %w", err)
		}
		if entry.IsDir() {
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

// TestRego runs the OPA tests found in the rego files under this package.
func TestRego(t *testing.T) {
	ctx := context.Background()

	mods, err := parseModules()
	if err != nil {
		t.Fatalf("Failed to parse test modules: %v", err)
	}

	rch, err := tester.NewRunner().
		SetModules(mods).
		EnableFailureLine(true).
		EnableTracing(true).
		RunTests(ctx, nil)
	if err != nil {
		t.Fatalf("Error running tests: %v", err)
	}

	if err := (tester.PrettyReporter{Output: os.Stderr, Verbose: true}).Report(rch); err != nil {
		t.Fatalf("Error in test: %v", err)
	}
}

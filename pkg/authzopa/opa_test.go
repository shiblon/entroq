package authzopa

import (
	"context"
	"fmt"
	"os"
	"testing"

	"embed"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/tester"
)

var (
	//go:embed regodata/*.rego
	regoContent embed.FS
)

func parseModules() (map[string]*ast.Module, error) {
	entries, err := regoContent.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	result := make(map[string]*ast.Module)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		b, err := regoContent.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("parse module: %w", err)
		}
		m, err := ast.ParseModule(entry.Name(), string(b))
		if err != nil {
			return nil, fmt.Errorf("parse module: %w", err)
		}
		result[entry.Name()] = m
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

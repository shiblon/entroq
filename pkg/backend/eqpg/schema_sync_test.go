package eqpg

import (
	"os"
	"regexp"
	"testing"
)

// TestSchemaFilesInSync enforces the invariant documented in AGENTS.md: the
// canonical Go schema and the vendored Python-client copy must be byte-identical,
// and the schema-version stamp, the Go SchemaVersion constant, and the Python
// SCHEMA_VERSION constant must all agree. Drift makes the Python client refuse to
// connect (version mismatch) and silently diverges behavior between backends.
//
// The Go schema is canonical; run `make schema-sync` after editing it. This test
// is the guard that keeps the copy from drifting.
func TestSchemaFilesInSync(t *testing.T) {
	const (
		canonical = "schema.sql"
		pyDir     = "../../../clients/py/src/entroq/pg"
	)
	pySchema := pyDir + "/schema.sql"

	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	match := func(s, pat, what string) string {
		m := regexp.MustCompile(pat).FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("could not find %s (pattern %q)", what, pat)
		}
		return m[1]
	}

	if goSQL, pySQL := read(canonical), read(pySchema); goSQL != pySQL {
		t.Errorf("%s and %s have drifted; they must be byte-identical.\n"+
			"Canonical is %s -- run `make schema-sync`.", canonical, pySchema, canonical)
	}

	// The version the schema stamps into entroq.meta must equal both clients'
	// compiled-in expectations.
	stamped := match(read(canonical), `'schema_version',\s*'([^']+)'`, "schema_version stamp")
	pyVer := match(read(pyDir+"/__init__.py"), `SCHEMA_VERSION\s*=\s*"([^"]+)"`, "SCHEMA_VERSION")

	if SchemaVersion != stamped {
		t.Errorf("Go SchemaVersion (%q) != schema stamp (%q); keep them in lockstep", SchemaVersion, stamped)
	}
	if pyVer != stamped {
		t.Errorf("Python SCHEMA_VERSION (%q) != schema stamp (%q); keep them in lockstep", pyVer, stamped)
	}
}

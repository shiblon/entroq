package eqpg_test

import (
	"regexp"
	"testing"

	"github.com/shiblon/entroq/pkg/backend/eqpg"
)

// TestSchemaSQLVersionMatchesConst guards the schema version against drifting
// between the two places it lives: the SchemaVersion constant (which initDB
// checks) and the literal schema.sql writes into entroq.meta. They are two
// copies of one fact; if they disagree, initDB compares the stored version
// against the wrong string. (This drift is exactly how the schema version once
// got ahead of the module version -- see SchemaVersion. The module >= schema
// invariant itself is a release-time gate, not a unit test, because during
// development the schema is legitimately ahead of the last tag.)
func TestSchemaSQLVersionMatchesConst(t *testing.T) {
	re := regexp.MustCompile(`'schema_version',\s*'([^']+)'`)
	m := re.FindStringSubmatch(eqpg.SchemaSQL)
	if m == nil {
		t.Fatal("could not find the schema_version literal in schema.sql")
	}
	if m[1] != eqpg.SchemaVersion {
		t.Errorf("schema.sql writes schema_version %q, but SchemaVersion const is %q", m[1], eqpg.SchemaVersion)
	}
}

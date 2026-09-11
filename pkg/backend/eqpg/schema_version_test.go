package eqpg

import (
	"regexp"
	"testing"
)

func TestSchemaVersion(t *testing.T) {
	match := regexp.MustCompile(`'schema_version',\s*'([^']+)'`).FindStringSubmatch(SchemaSQL)
	if match == nil {
		t.Fatal("schema.sql does not stamp schema_version")
	}
	if stamped := match[1]; SchemaVersion != stamped {
		t.Errorf("SchemaVersion %q does not match schema.sql stamp %q", SchemaVersion, stamped)
	}
}

package entroq

import (
	"encoding/json"
	"testing"
)

// TestDocSetJSONNamesItsFields pins DocSet's JSON field names, which match
// Doc's: changing one later would change what callers have stored or sent.
func TestDocSetJSONNamesItsFields(t *testing.T) {
	b, err := json.Marshal(&DocSet{Namespace: "ns", Key: "k", Version: 2, Claimant: "c", NumDocs: 1, Docs: []*Doc{{ID: "d"}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, k := range []string{"namespace", "key", "version", "claimant", "at", "num_docs", "docs"} {
		if _, ok := got[k]; !ok {
			t.Errorf("DocSet JSON lacks %q: %s", k, b)
		}
	}
	if len(got) != 7 {
		t.Errorf("DocSet JSON has %d fields, want 7: %s", len(got), b)
	}
}

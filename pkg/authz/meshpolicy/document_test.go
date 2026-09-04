package meshpolicy

import (
	"strings"
	"testing"
)

func TestDocumentValidate(t *testing.T) {
	valid := Document{
		Initialized: true,
		Queues: []QueuePolicy{{
			Pattern:        "/payments/api/inbox",
			MatchType:      "Exact",
			AllowedCallers: []map[string]string{{"app": "gateway"}},
		}},
		Namespaces: []NamespacePolicy{{
			Pattern:        "/payments/shared/",
			MatchType:      "Prefix",
			AllowedCallers: []map[string]string{{"team": "payments"}},
		}},
		Identities: map[string]Identity{"gateway": {Labels: map[string]string{"app": "gateway"}}},
	}

	for _, tc := range []struct {
		name string
		doc  Document
		want bool
	}{
		{name: "valid", doc: valid, want: true},
		{name: "not initialized", doc: Document{}},
		{name: "empty queue pattern", doc: Document{Initialized: true, Queues: []QueuePolicy{{MatchType: "Exact", AllowedCallers: []map[string]string{{"app": "a"}}}}}},
		{name: "unknown queue match", doc: Document{Initialized: true, Queues: []QueuePolicy{{Pattern: "/q", MatchType: "Regex", AllowedCallers: []map[string]string{{"app": "a"}}}}}},
		{name: "queue without callers", doc: Document{Initialized: true, Queues: []QueuePolicy{{Pattern: "/q", MatchType: "Exact"}}}},
		{name: "queue with empty matcher", doc: Document{Initialized: true, Queues: []QueuePolicy{{Pattern: "/q", MatchType: "Exact", AllowedCallers: []map[string]string{{}}}}}},
		{name: "empty namespace pattern", doc: Document{Initialized: true, Namespaces: []NamespacePolicy{{MatchType: "Prefix", AllowedCallers: []map[string]string{{"app": "a"}}}}}},
		{name: "unknown namespace match", doc: Document{Initialized: true, Namespaces: []NamespacePolicy{{Pattern: "/n", MatchType: "Glob", AllowedCallers: []map[string]string{{"app": "a"}}}}}},
		{
			name: "empty identity subject",
			doc: Document{
				Initialized: true,
				Identities:  map[string]Identity{"": {Labels: map[string]string{"app": "a"}}},
			},
		},
		{
			name: "identity without labels",
			doc: Document{
				Initialized: true,
				Identities:  map[string]Identity{"a": {}},
			},
		},
		{
			name: "invalid caller label key",
			doc: Document{
				Initialized: true,
				Queues: []QueuePolicy{{
					Pattern:        "/q",
					MatchType:      "Exact",
					AllowedCallers: []map[string]string{{"bad key": "value"}},
				}},
			},
		},
		{
			name: "invalid identity label value",
			doc: Document{
				Initialized: true,
				Identities: map[string]Identity{
					"a": {Labels: map[string]string{"app": strings.Repeat("x", 64)}},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.doc.Validate() == nil; got != tc.want {
				t.Fatalf("Validate() success = %v, want %v", got, tc.want)
			}
		})
	}
}

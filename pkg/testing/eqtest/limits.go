package eqtest

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// LengthLimits checks the byte-length bounds every backend enforces: 64 bytes
// for task and doc IDs and claimants, 1024 for doc namespaces, and 256 for
// doc keys. Over-limit values use two-byte characters, so a backend that
// counts characters instead of bytes accepts them and fails the test.
func LengthLimits(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()

	queue := path.Join(qPrefix, "length_limits")
	ns := path.Join(qPrefix, "length_limits")
	// wide returns n two-byte characters: n characters, 2n bytes.
	wide := func(n int) string { return strings.Repeat("é", n) }
	// nsOf returns a namespace under ns that is exactly n bytes long.
	nsOf := func(n int) string { return ns + "/" + strings.Repeat("n", n-len(ns)-1) }

	writes := []struct {
		name     string
		arg      entroq.ModifyArg
		claimant string // Modifying claimant, or the client default if empty.
		ok       bool
	}{
		{"task id at limit", entroq.InsertingInto(queue, entroq.WithID(uniqueTaskIDOfLen(64))), "", true},
		{"task id over limit", entroq.InsertingInto(queue, entroq.WithID(wide(33))), "", false},
		{"doc id at limit", entroq.PuttingDocInto(ns, entroq.WithIDKeys(strings.Repeat("a", 64), "k", "")), "", true},
		{"doc id over limit", entroq.PuttingDocInto(ns, entroq.WithIDKeys(wide(33), "k", "")), "", false},
		{"doc key at limit", entroq.PuttingDocInto(ns, entroq.WithIDKeys("", strings.Repeat("k", 256), "")), "", true},
		{"doc key over limit", entroq.PuttingDocInto(ns, entroq.WithIDKeys("", wide(129), "")), "", false},
		{"doc secondary key at limit", entroq.PuttingDocInto(ns, entroq.WithIDKeys("", "k", strings.Repeat("s", 256))), "", true},
		{"doc secondary key over limit", entroq.PuttingDocInto(ns, entroq.WithIDKeys("", "k", wide(129))), "", false},
		{"doc namespace at limit", entroq.PuttingDocInto(nsOf(1024), entroq.WithIDKeys("", "k", "")), "", true},
		{"doc namespace over limit", entroq.PuttingDocInto(nsOf(1023)+"é", entroq.WithIDKeys("", "k", "")), "", false},
		{"insert claimant at limit", entroq.InsertingInto(queue), strings.Repeat("c", 64), true},
		{"insert claimant over limit", entroq.InsertingInto(queue), wide(33), false},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			args := []entroq.ModifyArg{w.arg}
			if w.claimant != "" {
				args = append(args, entroq.ModifyAs(w.claimant))
			}
			_, err := client.Modify(ctx, args...)
			if w.ok && err != nil {
				t.Fatalf("rejected in-limit value: %v", err)
			}
			if !w.ok && err == nil {
				t.Fatal("accepted over-limit value")
			}
		})
	}

	t.Run("task claimant over limit", func(t *testing.T) {
		q := path.Join(queue, "claim")
		if _, err := client.Modify(ctx, entroq.InsertingInto(q)); err != nil {
			t.Fatal(err)
		}
		if task, err := client.TryClaim(ctx, entroq.From(q), entroq.ClaimFor(time.Minute), entroq.WithClaimant(wide(33))); err == nil {
			t.Fatalf("claimed with an over-limit claimant: %v", task)
		}
	})

	t.Run("task change claimant over limit", func(t *testing.T) {
		q := path.Join(queue, "change")
		resp, err := client.Modify(ctx, entroq.InsertingInto(q))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Modify(ctx, resp.InsertedTasks[0].Change(entroq.ArrivalTimeBy(time.Minute)), entroq.ModifyAs(wide(33))); err == nil {
			t.Fatal("changed a task with an over-limit claimant")
		}
	})

	t.Run("doc claimant over limit", func(t *testing.T) {
		dns := path.Join(ns, "claim")
		if _, err := client.Modify(ctx, entroq.PuttingDocInto(dns, entroq.WithIDKeys("", "k", ""))); err != nil {
			t.Fatal(err)
		}
		cq := entroq.ClaimKey(dns, "k").For(time.Minute)
		entroq.WithDocClaimant(wide(33))(cq)
		if docs, err := client.ClaimDocs(ctx, cq); err == nil {
			t.Fatalf("claimed docs with an over-limit claimant: %v", docs)
		}
	})

	// Deletes only reference an existing row, so they store no claimant.
	t.Run("delete ignores claimant length", func(t *testing.T) {
		q := path.Join(queue, "delete")
		resp, err := client.Modify(ctx, entroq.InsertingInto(q))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Modify(ctx, resp.InsertedTasks[0].Delete(), entroq.ModifyAs(wide(33))); err != nil {
			t.Fatalf("delete with a long claimant: %v", err)
		}
	})
}

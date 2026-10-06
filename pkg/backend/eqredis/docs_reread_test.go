package eqredis

import (
	"context"
	"fmt"
	"testing"

	"github.com/shiblon/entroq"
)

// TestDocsRereadsAfterADocMoves covers the one state readDocsWithLocks finds
// and cannot resolve on its own: a doc living in a set its caller's key map
// does not name, so the read covered no lock for it and the doc has no
// version. Docs reads the keys before the docs, so a doc deleted and inserted
// again at another key in between lands there.
//
// The reread has to converge, which it does only if the read that found the
// doc moved says where it went. The interleaving is sequenced here rather
// than raced for, since the key map is what carries it.
func TestDocsRereadsAfterADocMoves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	be, err := Open(ctx, WithAddr(redisAddr))
	if err != nil {
		t.Fatalf("Open backend: %v", err)
	}
	defer be.Close()

	client, err := entroq.New(ctx, Opener(WithAddr(redisAddr)))
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()

	ns := fmt.Sprintf("redistest/%s/docs_reread", client.GenID())
	const id = "mover"

	res, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithIDKeys(id, "before", "")))
	if err != nil {
		t.Fatalf("Insert at %q: %v", "before", err)
	}

	// What Docs would have read into its key map, before the doc moved.
	ids := []string{id}
	keys := map[string]string{id: "before"}

	if _, err := client.Modify(ctx, res.InsertedDocs[0].Delete()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithIDKeys(id, "after", ""))); err != nil {
		t.Fatalf("Insert at %q: %v", "after", err)
	}

	rq := &entroq.DocQuery{Namespace: ns, IDs: ids}

	// What the doc looks like read the ordinary way, which looks its key up
	// fresh and so never meets a stale map. A fresh set sits at version 0, so
	// the version a reread produces is worth comparing against something.
	want, err := client.Docs(ctx, rq)
	if err != nil {
		t.Fatalf("Docs: %v", err)
	}
	if len(want) != 1 {
		t.Fatalf("Docs: got %d docs, want 1", len(want))
	}

	docs, ok, err := be.readDocsWithLocks(ctx, rq, ids, keys)
	if err != nil {
		t.Fatalf("Read with a stale key map: %v", err)
	}
	if ok {
		t.Fatalf("Read with a stale key map: want a reread, got %d docs", len(docs))
	}
	if got := keys[id]; got != "after" {
		t.Fatalf("Key map after a read that found the doc moved: %q, want %q; a reread with this map finds the doc moved again, forever", got, "after")
	}

	docs, ok, err = be.readDocsWithLocks(ctx, rq, ids, keys)
	if err != nil {
		t.Fatalf("Reread: %v", err)
	}
	if !ok {
		t.Fatalf("Reread with the corrected key map: want it to resolve, got another reread")
	}
	if len(docs) != 1 {
		t.Fatalf("Reread: got %d docs, want 1", len(docs))
	}
	if docs[0].ID != id || docs[0].Key != "after" {
		t.Errorf("Reread: got doc %q at key %q, want %q at %q", docs[0].ID, docs[0].Key, id, "after")
	}
	if docs[0].Version != want[0].Version {
		t.Errorf("Reread: doc version %d, want %d, its set's version as an ordinary read reports it", docs[0].Version, want[0].Version)
	}
}

package eqredis

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
)

func docContent(ctx context.Context, t *testing.T, client *entroq.EntroQ, ns, id string) string {
	t.Helper()
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: ns, IDs: []string{id}})
	if err != nil {
		t.Fatalf("docs in %q: %v", ns, err)
	}
	if len(docs) != 1 {
		t.Fatalf("docs in %q: got %d, want 1", ns, len(docs))
	}
	var s string
	if err := json.Unmarshal(docs[0].Content, &s); err != nil {
		t.Fatalf("doc content in %q: %v", ns, err)
	}
	return s
}

// TestDocKeysDistinguishEscapedNamespaces checks that namespaces "a/b" and
// "a%2Fb" no longer share doc keys. Before "%" was escaped, a doc in one
// overwrote the doc with the same ID in the other.
func TestDocKeysDistinguishEscapedNamespaces(t *testing.T) {
	ctx := context.Background()
	client, err := redisClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// A fresh root per run keeps repeated runs against the shared Redis apart.
	root := "keycollide-" + entroq.GenHex16()
	slash, escaped := root+"/a/b", root+"/a%2Fb"
	if _, err := client.Modify(ctx,
		entroq.PuttingDocInto(slash, entroq.WithIDKeys("d", "k", ""), entroq.WithContent("slash")),
		entroq.PuttingDocInto(escaped, entroq.WithIDKeys("d", "k", ""), entroq.WithContent("escaped")),
	); err != nil {
		t.Fatal(err)
	}
	if got := docContent(ctx, t, client, slash, "d"); got != "slash" {
		t.Errorf("doc in %q = %q, want %q", slash, got, "slash")
	}
	if got := docContent(ctx, t, client, escaped, "d"); got != "escaped" {
		t.Errorf("doc in %q = %q, want %q", escaped, got, "escaped")
	}
}

// TestMigrateDocKeys stores docs under the legacy key encoding and checks
// that opening the backend moves them to their current keys, leaving alone a
// legacy key that holds the colliding namespace's doc.
func TestMigrateDocKeys(t *testing.T) {
	ctx := context.Background()
	client, err := redisClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	// "pct%" is the only namespace whose encoding changes. "col/x" and
	// "col%2Fx" collided under the legacy encoding; "col/x" keeps its key.
	root := "migrate-" + entroq.GenHex16()
	moved, kept, lost := root+"/pct%", root+"/col/x", root+"/col%2Fx"
	if _, err := client.Modify(ctx,
		entroq.PuttingDocInto(moved, entroq.WithIDKeys("d", "k", ""), entroq.WithContent("moved")),
		entroq.PuttingDocInto(kept, entroq.WithIDKeys("d", "k", ""), entroq.WithContent("kept")),
	); err != nil {
		t.Fatal(err)
	}
	// Put the "pct%" doc back at its legacy key, as an older server stored it.
	if err := rdb.Rename(ctx, docKey(moved, "d"), legacyDocKey(moved, "d")).Err(); err != nil {
		t.Fatal(err)
	}
	// Index "col%2Fx" as holding doc "d", whose legacy key is the "col/x" doc:
	// the state an older server left after one overwrote the other.
	if err := rdb.ZAdd(ctx, docNSIndexKey(lost), redis.Z{Member: docIndexMember("k", "", "d")}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, namespacesKey, lost).Err(); err != nil {
		t.Fatal(err)
	}
	defer rdb.Del(ctx, docNSIndexKey(lost))
	defer rdb.SRem(ctx, namespacesKey, lost)
	if legacyDocKey(lost, "d") != docKey(kept, "d") {
		t.Fatalf("test setup: %q and %q should collide under the legacy encoding", lost, kept)
	}

	reopened, err := redisClient(ctx)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if got := docContent(ctx, t, reopened, moved, "d"); got != "moved" {
		t.Errorf("doc in %q = %q, want %q", moved, got, "moved")
	}
	if n, err := rdb.Exists(ctx, legacyDocKey(moved, "d")).Result(); err != nil || n != 0 {
		t.Errorf("legacy key for %q still exists (n=%d, err=%v)", moved, n, err)
	}
	if got := docContent(ctx, t, reopened, kept, "d"); got != "kept" {
		t.Errorf("doc in %q = %q, want %q", kept, got, "kept")
	}
}

// TestMigrateDocLocks checks that opening a database written before doc groups
// had locks gives each group a lock one version past its highest member, with
// any claim released.
func TestMigrateDocLocks(t *testing.T) {
	ctx := context.Background()
	client, err := redisClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	ns := "migrate-locks-" + entroq.GenHex16()
	if _, err := client.Modify(ctx,
		entroq.PuttingDocInto(ns, entroq.WithIDKeys("a", "k", "1")),
		entroq.PuttingDocInto(ns, entroq.WithIDKeys("b", "k", "2")),
		entroq.PuttingDocInto(ns, entroq.WithIDKeys("c", "other", "")),
	); err != nil {
		t.Fatal(err)
	}

	// Put the namespace back the way an older server left it: per-doc versions
	// and claims, and no locks.
	future := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	for _, cmd := range []redis.Cmder{
		rdb.HSet(ctx, docKey(ns, "a"), "version", "2"),
		rdb.HSet(ctx, docKey(ns, "b"), "version", "7", "claimant", "holder", "at", future),
		rdb.HSet(ctx, docKey(ns, "c"), "version", "0"),
		rdb.Del(ctx, lockKey(docgroup.Group{Namespace: ns, Key: "k"}), lockKey(docgroup.Group{Namespace: ns, Key: "other"}),
			lockIndexKey(ns), heldGroupsKey(ns), docLocksMigratedKey, docFieldsMigratedKey),
	} {
		if err := cmd.Err(); err != nil {
			t.Fatalf("simulate legacy state: %v", err)
		}
	}

	reopened, err := redisClient(ctx)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	docs, err := reopened.Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil || len(docs) != 3 {
		t.Fatalf("docs after migration: %v, %v", docs, err)
	}
	want := map[string]int32{"a": 8, "b": 8, "c": 1}
	for _, d := range docs {
		if d.Version != want[d.ID] || d.Claimant != "" {
			t.Errorf("migrated doc %q: want version %d and no claimant, got version %d, claimant %q", d.ID, want[d.ID], d.Version, d.Claimant)
		}
	}
	if _, err := reopened.Modify(ctx, docs[0].Change(entroq.WithContent("after"))); err != nil {
		t.Errorf("change after migration: %v", err)
	}
	checkNoDocFields(ctx, t, rdb, ns, "a", "b", "c")
}

// checkNoDocFields fails for each doc in ns whose hash still holds its own
// version, claimant, or arrival time.
func checkNoDocFields(ctx context.Context, t *testing.T, rdb *redis.Client, ns string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		for _, field := range []string{"version", "claimant", "at"} {
			if ok, err := rdb.HExists(ctx, docKey(ns, id), field).Result(); err != nil || ok {
				t.Errorf("doc %q still has field %q (err %v)", id, field, err)
			}
		}
	}
}

// TestMigrateDocFieldsAfterLocks covers a database a development build left
// with group locks, and the lock migration marked done, but with doc hashes
// still holding their own versions and claims. The fields go; the locks stay.
func TestMigrateDocFieldsAfterLocks(t *testing.T) {
	ctx := context.Background()
	client, err := redisClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	ns := "migrate-fields-" + entroq.GenHex16()
	resp, err := client.Modify(ctx, entroq.PuttingDocInto(ns, entroq.WithIDKeys("a", "k", "")))
	if err != nil {
		t.Fatal(err)
	}
	version := resp.InsertedDocs[0].Version
	for _, cmd := range []redis.Cmder{
		rdb.HSet(ctx, docKey(ns, "a"), "version", "99", "claimant", "", "at", "0"),
		rdb.Del(ctx, docFieldsMigratedKey),
	} {
		if err := cmd.Err(); err != nil {
			t.Fatalf("simulate development state: %v", err)
		}
	}

	reopened, err := redisClient(ctx)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	checkNoDocFields(ctx, t, rdb, ns, "a")
	docs, err := reopened.Docs(ctx, &entroq.DocQuery{Namespace: ns})
	if err != nil || len(docs) != 1 || docs[0].Version != version {
		t.Errorf("doc after field migration: want version %d from its lock, got %v, %v", version, docs, err)
	}
}

// TestMigrateClaimsIndex puts a queue back the way an earlier version left it,
// with no claims index and a never-claimed task in the claimed set, and checks
// that opening builds the index and corrects the counts.
func TestMigrateClaimsIndex(t *testing.T) {
	ctx := context.Background()
	client, err := redisClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	queue := "migrate-claims-" + entroq.GenHex16()
	resp, err := client.Modify(ctx, entroq.InsertingInto(queue), entroq.InsertingInto(queue))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(time.Hour))
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	var never *entroq.Task
	for _, task := range resp.InsertedTasks {
		if task.ID != claimed.ID {
			never = task
		}
	}
	later := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	for _, cmd := range []redis.Cmder{
		rdb.HSet(ctx, taskKey(never.ID), "at", later),
		rdb.ZAdd(ctx, queueKey(queue), redis.Z{Score: float64(time.Now().Add(time.Hour).UnixMilli()), Member: never.ID}),
		rdb.ZAdd(ctx, qsclaimedKey(queue), redis.Z{Score: float64(time.Now().Add(time.Hour).UnixMilli()), Member: never.ID}),
		rdb.Del(ctx, qsclaimsKey(queue), claimsIndexMigratedKey),
	} {
		if err := cmd.Err(); err != nil {
			t.Fatalf("simulate legacy state: %v", err)
		}
	}

	reopened, err := redisClient(ctx)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	stats, err := reopened.QueueStats(ctx, entroq.MatchExact(queue))
	if err != nil {
		t.Fatal(err)
	}
	s := stats[queue]
	if s == nil || s.Claimed != 1 || s.Future != 1 || s.MaxClaims != 1 {
		t.Errorf("stats after migration: want 1 claimed, 1 future, max claims 1; got %+v", s)
	}
}

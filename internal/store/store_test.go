package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// keyHashVersion is the schema version before keys were identified by
// their hash.
const keyHashVersion = 4

// TestKeyHashMigration moves a version-4 database's requests and quota
// use from key names to key hashes.
func TestKeyHashMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:keyHashVersion] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256([]byte("sk-agyrelay-test"))
	seed := []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, keyHashVersion),
		fmt.Sprintf(`INSERT INTO api_keys (name, sha256, note, models, limits, created_at) VALUES ('alice', x'%x', '', '[]', '{}', 1)`, sum),
		`INSERT INTO requests (at, key_name, account, model, slug, api, stream, mode, status, error, finish, duration_ms, input, cached, output, reasoning, quota_group)
			VALUES (1000, 'alice', 'main', 'm', 's', 'chat', 0, 'fresh', 'ok', '', 'stop', 1, 10, 0, 1, 0, 'gemini')`,
		`INSERT INTO key_usage (key_name, grp, used) VALUES ('alice', 'gemini', 0.25)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	hash := hex.EncodeToString(sum[:])

	keys, err := st.Keys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Hash != hash || keys[0].Name != "alice" {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	rows, err := st.Requests(ctx, RequestFilter{KeyHash: hash}, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].KeyName != "alice" {
		t.Fatalf("requests = %+v, %v", rows, err)
	}
	uses, err := st.KeyUses(ctx, hash)
	if err != nil || len(uses) != 1 || uses[0].Used != 0.25 {
		t.Fatalf("key uses = %+v, %v", uses, err)
	}
}

func TestDeleteKey(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	id, err := st.AddKey(ctx, Key{Name: "alice", Hash: "h1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddRequest(ctx, Request{At: time.Now(), KeyHash: "h1", KeyName: "alice", QuotaGroup: "gemini", Input: 1}); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.Requests(ctx, RequestFilter{}, 0, 1)
	if err := st.AttributeQuota(ctx, "main", "b", time.Now(), map[int64]float64{rows[0].ID: 0.1}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.DeleteKey(ctx, id); !errors.Is(err, ErrKeyActive) {
		t.Fatalf("delete an active key: %v", err)
	}
	if err := st.RevokeKey(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if name, hash, err := st.DeleteKey(ctx, id); err != nil || name != "alice" || hash != "h1" {
		t.Fatalf("delete = %q %q %v", name, hash, err)
	}
	if uses, err := st.KeyUses(ctx, "h1"); err != nil || len(uses) != 0 {
		t.Fatalf("quota use after delete = %+v, %v", uses, err)
	}
	if rows, err := st.Requests(ctx, RequestFilter{KeyHash: "h1"}, 0, 10); err != nil || len(rows) != 1 {
		t.Fatalf("requests after delete = %+v, %v (they stay)", rows, err)
	}
	newID, err := st.AddKey(ctx, Key{Name: "alice", Hash: "h2", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("reuse the name: %v", err)
	}
	if newID == id {
		t.Fatalf("the new key reuses the deleted key's id %d", id)
	}
}

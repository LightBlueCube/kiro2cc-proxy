package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteKey(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.AddKey(ctx, Key{Hash: "h1", Name: "alice", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRequest(ctx, Request{At: time.Now(), KeyHash: "h1", KeyName: "alice", QuotaGroup: "gemini", Input: 1}); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.Requests(ctx, RequestFilter{}, 0, 1)
	if err := st.AttributeQuota(ctx, "main", "b", time.Now(), map[int64]float64{rows[0].ID: 0.1}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.DeleteKey(ctx, "h1"); !errors.Is(err, ErrKeyActive) {
		t.Fatalf("delete an active key: %v", err)
	}
	if err := st.RevokeKey(ctx, "h1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if name, err := st.DeleteKey(ctx, "h1"); err != nil || name != "alice" {
		t.Fatalf("delete = %q %v", name, err)
	}
	if _, err := st.DeleteKey(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
	if uses, err := st.KeyUses(ctx, "h1"); err != nil || len(uses) != 0 {
		t.Fatalf("quota use after delete = %+v, %v", uses, err)
	}
	if rows, err := st.Requests(ctx, RequestFilter{KeyHash: "h1"}, 0, 10); err != nil || len(rows) != 1 {
		t.Fatalf("requests after delete = %+v, %v (they stay)", rows, err)
	}
	if err := st.AddKey(ctx, Key{Hash: "h2", Name: "alice", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("reuse the name: %v", err)
	}
}

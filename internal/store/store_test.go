package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestKeyTotals(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	for _, r := range []Request{
		{KeyHash: "h1", Model: "small", Status: StatusOK, Input: 10, Cached: 4, Output: 2},
		{KeyHash: "h1", Model: "small", Status: StatusOK, Input: 20, Output: 3},
		{KeyHash: "h1", Model: "big", Status: StatusOK, Input: 100, Output: 50},
		{KeyHash: "h1", Model: "big", Status: "quota"},
		{KeyHash: "h2", Model: "small", Status: "context_length"},
	} {
		r.At = time.Now()
		if err := st.AddRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := st.KeyTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyTotal{
		{KeyHash: "h1", Model: "big", Requests: 1, Input: 100, Output: 50},
		{KeyHash: "h1", Model: "small", Requests: 2, Input: 30, Output: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("totals after prune = %+v, want %+v (failures do not count)", got, want)
	}
}

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
	cost := 0.5
	if err := st.AddRequest(ctx, Request{At: time.Now(), KeyHash: "h1", KeyName: "alice", Model: "m", Status: StatusOK, QuotaGroup: "gemini", Input: 1, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	if spends, err := st.KeySpends(ctx, "h1"); err != nil || len(spends) != 1 || spends[0].CostUSD != 0.5 {
		t.Fatalf("spend = %+v, %v", spends, err)
	}
	if totals, err := st.KeyTotals(ctx); err != nil || len(totals) != 1 {
		t.Fatalf("totals = %+v, %v", totals, err)
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
	if spends, err := st.KeySpends(ctx, "h1"); err != nil || len(spends) != 0 {
		t.Fatalf("spend after delete = %+v, %v", spends, err)
	}
	if totals, err := st.KeyTotals(ctx); err != nil || len(totals) != 0 {
		t.Fatalf("totals after delete = %+v, %v", totals, err)
	}
	if rows, err := st.Requests(ctx, RequestFilter{KeyHash: "h1"}, 0, 10); err != nil || len(rows) != 1 {
		t.Fatalf("requests after delete = %+v, %v (they stay)", rows, err)
	}
	if err := st.AddKey(ctx, Key{Hash: "h2", Name: "alice", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("reuse the name: %v", err)
	}
}

func TestAccountRouting(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.AddAccount(ctx, Account{Name: "main", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.Accounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].Priority != 0 || len(accounts[0].Models) != 0 {
		t.Fatalf("initial account = %+v, %v", accounts, err)
	}
	if err := st.SetAccountRouting(ctx, "main", 3, []string{"gemini-3.8-flash"}); err != nil {
		t.Fatal(err)
	}
	accounts, err = st.Accounts(ctx)
	if err != nil || accounts[0].Priority != 3 || !reflect.DeepEqual(accounts[0].Models, []string{"gemini-3.8-flash"}) {
		t.Fatalf("updated account = %+v, %v", accounts, err)
	}
	if err := st.SetAccountRouting(ctx, "nobody", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account = %v", err)
	}
}

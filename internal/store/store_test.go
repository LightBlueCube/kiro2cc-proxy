package store

import (
	"context"
	"database/sql"
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
		{KeyHash: "h1", Model: "big", Status: StatusCancelled},
		{KeyHash: "h1", Model: "big", Status: "quota"},
		{KeyHash: "h2", Model: "small", Status: "context_length"},
	} {
		r.At = time.Now()
		if err := st.AddRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := st.Summarize(ctx, RequestFilter{KeyHash: "h1"}, ByKey, 0)
	if err != nil || len(sums) != 1 || sums[0].Requests != 5 || sums[0].Errors != 1 {
		t.Fatalf("Summarize(h1) = %+v, %v; want 5 requests, 1 error (cancelled excluded)", sums, err)
	}
	if err := st.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := st.KeyTotals(ctx, "")
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
	gotOne, err := st.KeyTotals(ctx, "h1")
	if err != nil || !reflect.DeepEqual(gotOne, want) {
		t.Fatalf("KeyTotals(h1) = %+v, %v; want %+v", gotOne, err, want)
	}
	if gotOther, err := st.KeyTotals(ctx, "h2"); err != nil || len(gotOther) != 0 {
		t.Fatalf("KeyTotals(h2) = %+v, %v; want empty", gotOther, err)
	}
}

func TestKeyQuotas(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	for _, r := range []Request{
		{At: now, KeyHash: "h1", Account: "a1", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 10, Output: 2},
		{At: now, KeyHash: "h1", Account: "a2", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 10, Output: 2},
		{At: now, KeyHash: "h1", Account: "a2", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 10, Output: 2},
		{At: now, KeyHash: "h1", Account: "a1", QuotaGroup: "claude-gpt", Model: "m", Status: StatusOK, Input: 10, Output: 2},
		{At: now, KeyHash: "h2", Account: "a1", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 10, Output: 2},
	} {
		if err := st.AddRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err != nil || len(rows) != 5 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	// Rows are newest first: [h2/a1/gemini, h1/a1/claude-gpt, h1/a2/gemini, h1/a2/gemini, h1/a1/gemini].
	if err := st.AttributeQuota(ctx, "a1", "gemini-weekly", now.Add(time.Minute), map[int64]float64{
		rows[4].ID: 0.10,
		rows[0].ID: 0.05,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AttributeQuota(ctx, "a2", "gemini-weekly", now.Add(time.Minute), map[int64]float64{
		rows[3].ID: 0.25,
		rows[2].ID: 0.00, // zero share must not create a row
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AttributeQuota(ctx, "a1", "3p-weekly", now.Add(time.Minute), map[int64]float64{
		rows[1].ID: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	// Already-attributed request cannot be attributed twice.
	if err := st.AttributeQuota(ctx, "a1", "gemini-weekly", now.Add(2*time.Minute), map[int64]float64{
		rows[4].ID: 0.10,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate attribution = %v, want ErrNotFound", err)
	}

	if err := st.Prune(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := st.KeyQuotas(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyQuota{
		{KeyHash: "h1", Account: "a1", AccountName: "a1", Group: "claude-gpt", Used: 0.30},
		{KeyHash: "h1", Account: "a2", AccountName: "a2", Group: "gemini", Used: 0.25},
		{KeyHash: "h1", Account: "a1", AccountName: "a1", Group: "gemini", Used: 0.10},
		{KeyHash: "h2", Account: "a1", AccountName: "a1", Group: "gemini", Used: 0.05},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("quotas after prune = %+v, want %+v", got, want)
	}
	gotOne, err := st.KeyQuotas(ctx, "h1")
	if err != nil || !reflect.DeepEqual(gotOne, want[:3]) {
		t.Fatalf("KeyQuotas(h1) = %+v, %v; want %+v", gotOne, err, want[:3])
	}
}

func TestDeleteKey(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	maxWorkers := 3
	if err := st.AddKey(ctx, Key{Hash: "h1", Name: "alice", MaxWorkers: &maxWorkers, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if keys, err := st.Keys(ctx); err != nil || len(keys) != 1 || keys[0].MaxWorkers == nil || *keys[0].MaxWorkers != 3 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if err := st.UpdateKey(ctx, "h1", KeyUpdate{Note: "n"}); err != nil {
		t.Fatal(err)
	}
	if keys, err := st.Keys(ctx); err != nil || keys[0].MaxWorkers != nil {
		t.Fatalf("after clearing max_workers: %+v, %v", keys, err)
	}
	cost := 0.5
	if err := st.AddRequest(ctx, Request{At: time.Now(), KeyHash: "h1", KeyName: "alice", Account: "main", Model: "m", Status: StatusOK, QuotaGroup: "gemini", Input: 1, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Requests(ctx, RequestFilter{KeyHash: "h1"}, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("requests = %+v, %v", rows, err)
	}
	if err := st.AttributeQuota(ctx, "main", "gemini-weekly", time.Now(), map[int64]float64{rows[0].ID: 0.2}); err != nil {
		t.Fatal(err)
	}
	if spends, err := st.KeySpends(ctx, "h1"); err != nil || len(spends) != 1 || spends[0].CostUSD != 0.5 || spends[0].Group != "gemini" || rows[0].QuotaGroup != "gemini" {
		t.Fatalf("spend = %+v, %v; request group %q", spends, err, rows[0].QuotaGroup)
	}
	if totals, err := st.KeyTotals(ctx, ""); err != nil || len(totals) != 1 {
		t.Fatalf("totals = %+v, %v", totals, err)
	}
	if quotas, err := st.KeyQuotas(ctx, "h1"); err != nil || len(quotas) != 1 || quotas[0].Used != 0.2 {
		t.Fatalf("quotas = %+v, %v", quotas, err)
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
	if totals, err := st.KeyTotals(ctx, ""); err != nil || len(totals) != 0 {
		t.Fatalf("totals after delete = %+v, %v", totals, err)
	}
	if quotas, err := st.KeyQuotas(ctx, "h1"); err != nil || len(quotas) != 0 {
		t.Fatalf("quotas after delete = %+v, %v", quotas, err)
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
	if err != nil || len(accounts) != 1 || accounts[0].ID == "" || accounts[0].Priority != 0 || len(accounts[0].Models) != 0 {
		t.Fatalf("initial account = %+v, %v", accounts, err)
	}
	id := accounts[0].ID
	if err := st.SetAccountRouting(ctx, id, 3, []string{"gemini-3.8-flash"}); err != nil {
		t.Fatal(err)
	}
	accounts, err = st.Accounts(ctx)
	if err != nil || accounts[0].Priority != 3 || !reflect.DeepEqual(accounts[0].Models, []string{"gemini-3.8-flash"}) {
		t.Fatalf("updated account = %+v, %v", accounts, err)
	}
	if err := st.SetAccountRouting(ctx, "nobody", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account = %v", err)
	}

	const reason = "FAILED_PRECONDITION (code 400): User location is not supported for the API use."
	if err := st.SetAccountPaused(ctx, id, true, reason); err != nil {
		t.Fatal(err)
	}
	accounts, err = st.Accounts(ctx)
	if err != nil || !accounts[0].Paused || accounts[0].PauseReason != reason {
		t.Fatalf("paused account = %+v, %v", accounts, err)
	}
	if err := st.SetAccountPaused(ctx, id, false, reason); err != nil {
		t.Fatal(err)
	}
	accounts, err = st.Accounts(ctx)
	if err != nil || accounts[0].Paused || accounts[0].PauseReason != "" {
		t.Fatalf("resumed account = %+v, %v", accounts, err)
	}
}

func TestRenameAccount(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	if err := st.AddAccount(ctx, Account{ID: "id-1", Name: "main", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAccount(ctx, Account{ID: "id-2", Name: "backup", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRequest(ctx, Request{At: now, KeyHash: "h1", KeyName: "alice", Account: "id-1", AccountName: "main", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 10, Output: 5, CallToken: "tok1"}); err != nil {
		t.Fatal(err)
	}
	reqs, _ := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err := st.AttributeQuota(ctx, "id-1", "gemini-weekly", now.Add(time.Minute), map[int64]float64{reqs[0].ID: 0.2}); err != nil {
		t.Fatal(err)
	}
	if err := st.FlagPrompt(ctx, FlaggedPrompt{Fingerprint: "fp1", Text: "p", LastAt: now, Until: now.Add(time.Hour), KeyHash: "h1", KeyName: "alice", Account: "id-1", AccountName: "main", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddUpstreamCall(ctx, UpstreamCall{At: now, CallToken: "tok1", Account: "id-1", AccountName: "main", Session: "S1", Seq: 1, Model: "m", Status: 200}); err != nil {
		t.Fatal(err)
	}

	if err := st.RenameAccount(ctx, "id-1", "backup"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name = %v, want ErrExists", err)
	}
	if err := st.RenameAccount(ctx, "missing", "next"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id = %v, want ErrNotFound", err)
	}
	if err := st.RenameAccount(ctx, "id-1", "Alice Pro (主力)"); err != nil {
		t.Fatal(err)
	}

	// Removing the account afterwards must still keep the renamed display name
	// on historical requests, key_quota, flagged prompts, and upstream calls.
	if err := st.RemoveAccount(ctx, "id-1"); err != nil {
		t.Fatal(err)
	}
	reqs, err = st.Requests(ctx, RequestFilter{Account: "id-1"}, 0, 10)
	if err != nil || len(reqs) != 1 || reqs[0].AccountName != "Alice Pro (主力)" {
		t.Fatalf("requests after rename+remove = %+v, %v", reqs, err)
	}
	quotas, err := st.KeyQuotas(ctx, "h1")
	if err != nil || len(quotas) != 1 || quotas[0].Account != "id-1" || quotas[0].AccountName != "Alice Pro (主力)" {
		t.Fatalf("key_quota after rename+remove = %+v, %v", quotas, err)
	}
	flagged, err := st.FlaggedPrompts(ctx)
	if err != nil || len(flagged) != 1 || flagged[0].AccountName != "Alice Pro (主力)" {
		t.Fatalf("flagged after rename+remove = %+v, %v", flagged, err)
	}
	up, err := st.UpstreamCalls(ctx, reqs[0].ID)
	if err != nil || len(up) != 1 || up[0].AccountName != "Alice Pro (主力)" {
		t.Fatalf("upstream after rename+remove = %+v, %v", up, err)
	}
	filterAccounts, err := st.RequestAccounts(ctx, RequestFilter{})
	if err != nil || !reflect.DeepEqual(filterAccounts, []RequestAccount{{ID: "id-1", Name: "Alice Pro (主力)"}}) {
		t.Fatalf("RequestAccounts = %+v, %v", filterAccounts, err)
	}
}

func TestRequestKeepsItsSession(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.AddRequest(ctx, Request{At: time.Now(), Status: StatusOK, Session: "S1"}); err != nil {
		t.Fatal(err)
	}
	reqs, err := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err != nil || len(reqs) != 1 || reqs[0].Session != "S1" {
		t.Fatalf("requests = %+v, %v, want session S1", reqs, err)
	}
}

func TestAccountGroupUsage(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	since := time.Now().Truncate(time.Second)
	p1, p2 := 1.5, 0.25
	rows := []Request{
		// Before the window: excluded.
		{At: since.Add(-time.Minute), Duration: time.Second, Account: "main", QuotaGroup: "claude-gpt", Model: "claude-sonnet-4-6", Status: StatusOK, Input: 1000, Output: 100, CostUSD: &p1},
		// Started just before since, finished after since: included.
		{At: since.Add(-500 * time.Millisecond), Duration: time.Second, Account: "main", QuotaGroup: "claude-gpt", Model: "gpt-oss-120b", Status: StatusOK, Input: 200, Cached: 50, Output: 20, CostUSD: &p2},
		// Higher cost model + an unpriced call.
		{At: since.Add(time.Minute), Duration: time.Second, Account: "main", QuotaGroup: "claude-gpt", Model: "claude-sonnet-4-6", Status: StatusOK, Input: 500, Cached: 100, Output: 50, Reasoning: 10, CostUSD: &p1},
		{At: since.Add(2 * time.Minute), Duration: time.Second, Account: "main", QuotaGroup: "claude-gpt", Model: "claude-sonnet-4-6", Status: StatusOK, Input: 300, Output: 30},
		// Zero-token failure: excluded.
		{At: since.Add(3 * time.Minute), Account: "main", QuotaGroup: "claude-gpt", Model: "claude-sonnet-4-6", Status: "quota"},
		// Other group and other account: excluded.
		{At: since.Add(time.Minute), Account: "main", QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: StatusOK, Input: 999, Output: 99, CostUSD: &p1},
		{At: since.Add(time.Minute), Account: "other", QuotaGroup: "claude-gpt", Model: "claude-sonnet-4-6", Status: StatusOK, Input: 999, Output: 99, CostUSD: &p1},
	}
	for _, r := range rows {
		if err := st.AddRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.AccountGroupUsage(ctx, "main", "claude-gpt", since)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelUsage{
		{Model: "claude-sonnet-4-6", Requests: 2, Input: 800, Cached: 100, Output: 80, Reasoning: 10, CostUSD: 1.5, Unpriced: 1},
		{Model: "gpt-oss-120b", Requests: 1, Input: 200, Cached: 50, Output: 20, CostUSD: 0.25},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestExpirePendingQuota(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	cutoff := now.Add(-time.Hour)
	for _, r := range []Request{
		// Ended before cutoff, unsettled, has tokens -> should expire.
		{At: cutoff.Add(-time.Minute), Duration: 10 * time.Second, Account: "a", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 100},
		// Started before cutoff, ended after cutoff -> stays pending.
		{At: cutoff.Add(-time.Minute), Duration: 2 * time.Minute, Account: "a", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 100},
		// Ended before cutoff, zero tokens -> stays empty.
		{At: cutoff.Add(-2 * time.Minute), Account: "a", QuotaGroup: "gemini", Model: "m", Status: "quota"},
		// Ended before cutoff, already skipped -> keeps original reason.
		{At: cutoff.Add(-3 * time.Minute), Account: "b", QuotaGroup: "gemini", Model: "m", Status: StatusOK, Input: 100},
	} {
		if err := st.AddRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SkipQuota(ctx, "b", "gemini", now, SkipReset); err != nil {
		t.Fatal(err)
	}

	n, err := st.ExpirePendingQuota(ctx, cutoff, SkipFailed)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	if unsettled, err := st.HasUnsettledQuota(ctx, "a"); err != nil || !unsettled {
		t.Fatalf("a unsettled = %v, %v (recent pending must remain)", unsettled, err)
	}
	if unsettled, err := st.HasUnsettledQuota(ctx, "b"); err != nil || unsettled {
		t.Fatalf("b unsettled = %v, %v", unsettled, err)
	}

	rows, err := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	// Ordered newest inserted (id DESC) first.
	wantSkips := []QuotaSkip{SkipReset, "", "", SkipFailed}
	for i, want := range wantSkips {
		if rows[i].QuotaSkip != want {
			t.Fatalf("row %d quota_skip = %q, want %q", i, rows[i].QuotaSkip, want)
		}
	}
}

func TestOpenInitializesMissingColumnsAndTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an older schema already at user_version = 1, missing newer
	// columns (accounts.priority, accounts.models, api_keys.max_workers,
	// api_keys.limit_total, api_keys.limits, requests.effort,
	// requests.quota_skip) and missing tables (key_totals, quota_cursors).
	_, err = rawDB.Exec(`
		PRAGMA user_version = 1;
		CREATE TABLE accounts (
			name       TEXT PRIMARY KEY,
			env        TEXT NOT NULL,
			proxy      TEXT NOT NULL,
			paused     INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
		INSERT INTO accounts (name, env, proxy, paused, created_at) VALUES ('main', '', '', 0, 1000);
		CREATE TABLE api_keys (
			hash       TEXT PRIMARY KEY,
			name       TEXT NOT NULL UNIQUE,
			note       TEXT NOT NULL,
			models     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER
		);
		INSERT INTO api_keys (hash, name, note, models, created_at) VALUES ('h1', 'alice', '', '', 1000);
		CREATE TABLE requests (
			id            INTEGER PRIMARY KEY,
			at            INTEGER NOT NULL,
			key_hash      TEXT NOT NULL,
			key_name      TEXT NOT NULL,
			account       TEXT NOT NULL,
			model         TEXT NOT NULL,
			slug          TEXT NOT NULL,
			api           TEXT NOT NULL,
			stream        INTEGER NOT NULL,
			mode          TEXT NOT NULL,
			replay_reason TEXT NOT NULL,
			status        TEXT NOT NULL,
			error         TEXT NOT NULL,
			finish        TEXT NOT NULL,
			duration_ms   INTEGER NOT NULL,
			ttft_ms       INTEGER,
			input         INTEGER NOT NULL,
			cached        INTEGER NOT NULL,
			output        INTEGER NOT NULL,
			reasoning     INTEGER NOT NULL,
			cost_usd      REAL,
			quota_group   TEXT NOT NULL,
			quota_used    REAL
		);
		INSERT INTO requests (at, key_hash, key_name, account, model, slug, api, stream, mode, replay_reason, status, error, finish, duration_ms, input, cached, output, reasoning, quota_group, quota_used)
			VALUES
				(1000, 'h1', 'alice', 'main', 'm', 'm', 'chat', 0, 'fresh', '', 'ok', '', 'stop', 100, 10, 0, 5, 0, 'gemini', 0.15),
				(2000, 'h1', 'alice', 'main', 'm', 'm', 'chat', 0, 'fresh', '', 'ok', '', 'stop', 100, 10, 0, 5, 0, 'gemini', 0.10),
				(3000, 'gone', 'old', 'main', 'm', 'm', 'chat', 0, 'fresh', '', 'ok', '', 'stop', 100, 10, 0, 5, 0, 'gemini', 0.50);
	`)
	rawDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	accounts, err := st.Accounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].ID == "" || accounts[0].Name != "main" || accounts[0].Priority != 0 || len(accounts[0].Models) != 0 || len(accounts[0].Env) != 0 {
		t.Fatalf("accounts after upgrade = %+v, %v", accounts, err)
	}
	keys, err := st.Keys(ctx)
	if err != nil || len(keys) != 1 || keys[0].MaxWorkers != nil || keys[0].LimitTotal != nil || len(keys[0].Limits) != 0 || len(keys[0].Models) != 0 {
		t.Fatalf("keys after upgrade = %+v, %v", keys, err)
	}
	reqs, err := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err != nil || len(reqs) != 3 || reqs[0].Effort != "" || reqs[0].QuotaSkip != "" || reqs[0].Anchor != "" {
		t.Fatalf("requests after upgrade = %+v, %v", reqs, err)
	}
	if totals, err := st.KeyTotals(ctx, ""); err != nil || len(totals) != 0 {
		t.Fatalf("key_totals after upgrade = %+v, %v", totals, err)
	}
	wantQuotas := []KeyQuota{{KeyHash: "h1", Account: accounts[0].ID, AccountName: "main", Group: "gemini", Used: 0.25}}
	if quotas, err := st.KeyQuotas(ctx, ""); err != nil || !reflect.DeepEqual(quotas, wantQuotas) {
		t.Fatalf("key_quota after upgrade = %+v, %v; want %+v", quotas, err, wantQuotas)
	}
	st.Close()

	// Reopening must not re-run the one-time backfill.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if quotas, err := st2.KeyQuotas(ctx, ""); err != nil || !reflect.DeepEqual(quotas, wantQuotas) {
		t.Fatalf("key_quota after reopen = %+v, %v; want %+v", quotas, err, wantQuotas)
	}
}

func TestLatestQuotaSamples(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	t0 := time.Now().Truncate(time.Second)
	reset1 := t0.Add(7 * 24 * time.Hour)
	reset2 := reset1.Add(time.Hour)
	samples := []QuotaSample{
		{Account: "a1", At: t0, Bucket: "gemini-weekly", Group: "Gemini", Window: "weekly", Remaining: 1.0, ResetAt: reset1},
		{Account: "a1", At: t0.Add(time.Minute), Bucket: "gemini-weekly", Group: "Gemini", Window: "weekly", Remaining: 0.95, ResetAt: reset2},
		{Account: "a1", At: t0, Bucket: "3p-weekly", Group: "Claude", Window: "weekly", Remaining: 1.0, ResetAt: reset1},
		{Account: "a2", At: t0.Add(2 * time.Minute), Bucket: "gemini-weekly", Group: "Gemini", Window: "weekly", Remaining: 0.5, ResetAt: reset1},
	}
	if err := st.AddQuotaSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}

	got, err := st.LatestQuotaSamples(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("LatestQuotaSamples len = %d, want 2: %+v", len(got), got)
	}
	if got[0].Bucket != "3p-weekly" || got[0].Remaining != 1.0 {
		t.Errorf("got[0] = %+v, want 3p-weekly remaining 1.0", got[0])
	}
	if got[1].Bucket != "gemini-weekly" || got[1].Remaining != 0.95 || !got[1].ResetAt.Equal(reset2) {
		t.Errorf("got[1] = %+v, want gemini-weekly remaining 0.95 reset %v", got[1], reset2)
	}
	if empty, err := st.LatestQuotaSamples(ctx, "missing"); err != nil || len(empty) != 0 {
		t.Fatalf("missing account samples = %+v, %v", empty, err)
	}
}

func TestSystemKeyWarmupExcludedFromKeyTables(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	cost := 0.002
	if err := st.AddRequest(ctx, Request{
		At: now, Duration: 2 * time.Second, KeyHash: SystemKeyWarmup, KeyName: "warmup",
		Account: "a1", AccountName: "acct1", Model: "gemini-3.8-flash", Slug: "gemini-3.8-flash-low", Effort: "low",
		Status: StatusOK, Finish: "stop",
		Input: 100, Output: 5, CostUSD: &cost, QuotaGroup: "gemini",
	}); err != nil {
		t.Fatal(err)
	}
	reqs, err := st.Requests(ctx, RequestFilter{KeyHash: SystemKeyWarmup}, 0, 10)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("warmup requests = %+v, %v", reqs, err)
	}
	if err := st.AttributeQuota(ctx, "a1", "gemini-weekly", now.Add(10*time.Minute), map[int64]float64{reqs[0].ID: 0.001}); err != nil {
		t.Fatal(err)
	}
	if spends, err := st.KeySpends(ctx, ""); err != nil || len(spends) != 0 {
		t.Fatalf("key_usage = %+v, %v; want empty for system key", spends, err)
	}
	if totals, err := st.KeyTotals(ctx, ""); err != nil || len(totals) != 0 {
		t.Fatalf("key_totals = %+v, %v; want empty for system key", totals, err)
	}
	if quotas, err := st.KeyQuotas(ctx, ""); err != nil || len(quotas) != 0 {
		t.Fatalf("key_quota = %+v, %v; want empty for system key", quotas, err)
	}

	// LastAccountGroupRequestEnd sees the warmup request and ignores zero-token/other groups.
	if err := st.AddRequest(ctx, Request{
		At: now.Add(time.Minute), Duration: time.Second, KeyHash: SystemKeyWarmup, KeyName: "warmup",
		Account: "a1", Model: "gemini-3.8-flash", Status: "upstream_error", QuotaGroup: "gemini",
	}); err != nil {
		t.Fatal(err)
	}
	lastEnd, err := st.LastAccountGroupRequestEnd(ctx, "a1", "gemini", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := now.Add(2 * time.Second)
	if !lastEnd.Equal(wantEnd) {
		t.Fatalf("LastAccountGroupRequestEnd = %v, want %v", lastEnd, wantEnd)
	}
	if none, err := st.LastAccountGroupRequestEnd(ctx, "a1", "claude-gpt", now.Add(-time.Minute)); err != nil || !none.IsZero() {
		t.Fatalf("claude-gpt LastAccountGroupRequestEnd = %v, %v; want zero", none, err)
	}
}

func TestLegacyArgonPoolMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	if err := st.AddAccount(ctx, Account{ID: "a1", Name: "pro", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE api_keys ADD COLUMN merge_argon INTEGER NOT NULL DEFAULT 1`,
		`INSERT INTO api_keys (hash, name, limits, merge_argon, created_at) VALUES ('h1', 'both', '{"gemini":5,"argon":3}', 0, 1)`,
		`INSERT INTO api_keys (hash, name, limits, merge_argon, created_at) VALUES ('h2', 'argon_only', '{"argon":2}', 0, 2)`,
		`INSERT INTO key_usage (key_hash, grp, cost_usd) VALUES ('h1', 'gemini', 1.5), ('h1', 'argon', 2.25), ('h2', 'argon', 0.75)`,
		`INSERT INTO quota_samples (account, at, bucket, grp, win, remaining, reset_at) VALUES ('a1', 1000, 'gemini-argon-wtu-weekly', 'Gemini 4 Argon', 'weekly', 0.8, 2000), ('a1', 1000, 'gemini-weekly', 'Gemini', 'weekly', 0.9, 2000)`,
		`INSERT INTO quota_cursors (account, bucket, at) VALUES ('a1', 'gemini-argon-wtu-weekly', 1000), ('a1', 'gemini-weekly', 1000)`,
		`INSERT INTO key_quota (key_hash, account, account_name, grp, used) VALUES ('h1', 'a1', 'pro', 'argon', 0.2), ('h1', 'a1', 'pro', 'gemini', 0.1)`,
		`INSERT INTO requests (at, key_hash, key_name, account, account_name, model, status, input, output, quota_group, quota_used) VALUES (1000, 'h1', 'both', 'a1', 'pro', 'gemini-4-argon', 'ok', 10, 5, 'argon', 0.2)`,
		`INSERT INTO settings (name, value) VALUES ('settings', '{"warmup_models":{"gemini":"gemini-3.8-flash","argon":"gemini-4-argon"}}')`,
	} {
		if _, err := st.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	keys, err := st.Keys(ctx)
	if err != nil || len(keys) != 2 || len(keys[0].Limits) != 1 || keys[0].Limits["gemini"] != 5 || len(keys[1].Limits) != 0 {
		t.Fatalf("migrated keys = %+v, %v", keys, err)
	}
	spends, err := st.KeySpends(ctx, "")
	if err != nil || len(spends) != 2 {
		t.Fatalf("migrated spends = %+v, %v", spends, err)
	}
	gotSpend := map[string]float64{}
	for _, s := range spends {
		if s.Group != "gemini" {
			t.Fatalf("unexpected spend group: %+v", s)
		}
		gotSpend[s.KeyHash] = s.CostUSD
	}
	if gotSpend["h1"] != 3.75 || gotSpend["h2"] != 0.75 {
		t.Fatalf("folded spend = %+v", gotSpend)
	}
	samples, err := st.LatestQuotaSamples(ctx, "a1")
	if err != nil || len(samples) != 1 || samples[0].Bucket != "gemini-weekly" {
		t.Fatalf("migrated quota samples = %+v, %v", samples, err)
	}
	if _, err := st.QuotaCursor(ctx, "a1", "gemini-argon-wtu-weekly"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("argon cursor still present: %v", err)
	}
	quotas, err := st.KeyQuotas(ctx, "h1")
	if err != nil || len(quotas) != 1 || quotas[0].Group != "gemini" || quotas[0].Used != 0.1 {
		t.Fatalf("migrated key_quota = %+v, %v", quotas, err)
	}
	reqs, err := st.Requests(ctx, RequestFilter{}, 0, 10)
	if err != nil || len(reqs) != 1 || reqs[0].QuotaGroup != "gemini" || reqs[0].QuotaUsed != nil || reqs[0].QuotaSkip != SkipNoBucket {
		t.Fatalf("migrated requests = %+v, %v", reqs, err)
	}
	var saved struct {
		WarmupModels map[string]string `json:"warmup_models"`
	}
	if found, err := st.Setting(ctx, "settings", &saved); err != nil || !found || len(saved.WarmupModels) != 1 || saved.WarmupModels["gemini"] != "gemini-3.8-flash" {
		t.Fatalf("migrated settings = %+v (found=%v), %v", saved, found, err)
	}
}

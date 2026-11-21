package control

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"agyrelay/internal/cloudcode"
	"agyrelay/internal/egress"
	"agyrelay/internal/pool"
	"agyrelay/internal/promptflags"
	"agyrelay/internal/store"
)

func newTestControl(t *testing.T, st *store.Store, accountsDir string) (*Control, *pool.Pool) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pool.New(pool.Config{TurnsPerAccount: 1, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	eg, err := egress.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eg.Close() })
	cc, err := cloudcode.Start(cloudcode.DefaultUpstream, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	flags, err := promptflags.New(context.Background(), st, log)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := New(context.Background(), Config{
		Store:       st,
		Pool:        p,
		Egress:      eg,
		CloudCode:   cc,
		Flags:       flags,
		AccountsDir: accountsDir,
		Settings:    settings,
		Level:       new(slog.LevelVar),
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctl, p
}

func TestPauseAccountForLocationPersistsUntilManualResume(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "agyrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	accountsDir := filepath.Join(dir, "accounts")
	ctx := context.Background()

	ctl, p := newTestControl(t, st, accountsDir)
	acct, err := ctl.AddAccount(ctx, "main", nil, "http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}

	const reason = "FAILED_PRECONDITION (code 400): User location is not supported for the API use."
	ctl.pauseAccountForLocation(acct.ID(), reason)

	st1 := p.Account(acct.ID()).Status()
	if !st1.Paused || st1.PauseReason != reason {
		t.Fatalf("after auto-pause: status = %+v", st1)
	}

	// Changing proxy must not automatically resume the account.
	if err := ctl.SetAccountProxy(ctx, acct.ID(), "http://127.0.0.1:7891"); err != nil {
		t.Fatal(err)
	}
	st2 := p.Account(acct.ID()).Status()
	if !st2.Paused || st2.PauseReason != reason {
		t.Fatalf("after proxy change: status = %+v, want still paused with reason", st2)
	}

	// Restarting control restores both Paused and PauseReason from SQLite.
	ctl2, p2 := newTestControl(t, st, accountsDir)
	st3 := p2.Account(acct.ID()).Status()
	if !st3.Paused || st3.PauseReason != reason {
		t.Fatalf("after restart: status = %+v, want paused with reason", st3)
	}

	// Manual resume clears both Paused and PauseReason in memory and SQLite.
	if err := ctl2.SetAccountPaused(ctx, acct.ID(), false); err != nil {
		t.Fatal(err)
	}
	st4 := p2.Account(acct.ID()).Status()
	if st4.Paused || st4.PauseReason != "" {
		t.Fatalf("after manual resume: status = %+v, want active with no reason", st4)
	}
	records, err := ctl2.AccountRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if records[acct.ID()].Paused || records[acct.ID()].PauseReason != "" {
		t.Fatalf("stored after resume = %+v", records[acct.ID()])
	}
}

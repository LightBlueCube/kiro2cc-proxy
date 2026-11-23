package control

import (
	"context"
	"path/filepath"
	"testing"

	"agyrelay/internal/store"
)

func TestPauseAccountForBanPersistsUntilManualResume(t *testing.T) {
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

	const reason = "PERMISSION_DENIED (code 403): This service has been disabled in this account for violation of Terms of Service."
	ctl.pauseAccountForBan(acct.ID(), reason)

	if got := p.Account(acct.ID()).Status(); !got.Paused || got.PauseReason != reason {
		t.Fatalf("after ban: status = %+v, want paused with reason", got)
	}
	if err := p.Account(acct.ID()).Available("gemini"); err == nil {
		t.Fatal("a banned account must be out of rotation")
	}

	// Restarting control restores the pause from SQLite.
	ctl2, p2 := newTestControl(t, st, accountsDir)
	if got := p2.Account(acct.ID()).Status(); !got.Paused || got.PauseReason != reason {
		t.Fatalf("after restart: status = %+v, want paused with reason", got)
	}

	if err := ctl2.SetAccountPaused(ctx, acct.ID(), false); err != nil {
		t.Fatal(err)
	}
	if got := p2.Account(acct.ID()).Status(); got.Paused || got.PauseReason != "" {
		t.Fatalf("after manual resume: status = %+v, want active", got)
	}
}

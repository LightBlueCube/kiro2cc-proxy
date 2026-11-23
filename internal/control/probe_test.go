package control

import (
	"context"
	"errors"
	"testing"

	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/store"
)

func TestTestAccountRepliesAndRecordsUnderTestKey(t *testing.T) {
	e := newWarmupEnv(t)
	ctx := context.Background()

	acct, err := e.ctl.AddAccount(ctx, "main", []string{agytest.EnvRun + "=1"}, "")
	if err != nil {
		t.Fatal(err)
	}

	var input *InputError
	if _, err := e.ctl.TestAccount(ctx, acct.ID(), ""); !errors.As(err, &input) {
		t.Fatalf("test before the models are known: err = %v, want InputError", err)
	}

	if _, err := e.monitor.Refresh(ctx, acct); err != nil {
		t.Fatal(err)
	}

	chosen, err := e.ctl.TestAccount(ctx, acct.ID(), "")
	if err != nil || chosen.Reply != "reply to: who are you" || chosen.Slug == "" {
		t.Fatalf("test with the cheapest model = %+v, %v", chosen, err)
	}

	explicit, err := e.ctl.TestAccount(ctx, acct.ID(), "claude-opus-5-5")
	if err != nil || explicit.Model != "claude-opus-5-5" || explicit.Slug != "claude-opus-5-5-high" {
		t.Fatalf("test with claude-opus-5-5 = %+v, %v", explicit, err)
	}

	if _, err := e.ctl.TestAccount(ctx, acct.ID(), "no-such-model"); !errors.As(err, &input) {
		t.Fatalf("test with an unoffered model: err = %v, want InputError", err)
	}
	if _, err := e.ctl.TestAccount(ctx, "missing", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("test of an unknown account: err = %v, want ErrNotFound", err)
	}

	reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: store.SystemKeyTest}, 0, 10)
	if err != nil || len(reqs) != 2 {
		t.Fatalf("test requests = %+v, %v; want 2", reqs, err)
	}
	for _, r := range reqs {
		if r.KeyName != accountTestKeyName || r.Status != store.StatusOK || r.API != "" || r.Mode != "" {
			t.Errorf("unexpected test request row: %+v", r)
		}
	}
	if spends, err := e.st.KeySpends(ctx, ""); err != nil || len(spends) != 0 {
		t.Fatalf("key_usage = %+v, %v; want empty", spends, err)
	}

	if err := e.ctl.SetAccountPaused(ctx, acct.ID(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ctl.TestAccount(ctx, acct.ID(), ""); !errors.As(err, &input) {
		t.Fatalf("test of a paused account: err = %v, want InputError", err)
	}
}

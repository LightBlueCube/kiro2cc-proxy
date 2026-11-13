package pool

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
)

func TestProxyEnvAndBlocking(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	extra := []string{"X=1", "HTTPS_PROXY=http://ignored:1"}
	a, err := p.Add(AccountConfig{Name: "a", Home: filepath.Join(t.TempDir(), "a"), Env: extra, Proxy: "http://a:token@127.0.0.1:7890"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(extra), "HTTPS_PROXY=http://a:token@127.0.0.1:7890", "HTTP_PROXY=http://a:token@127.0.0.1:7890")
	if got := a.Env(); !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}

	a.SetBlocked(errors.New("no route"))
	if _, err := p.Pick("u", "gemini", nil); err == nil || a.Status().Blocked != "no route" {
		t.Fatalf("blocked account picked: %v", err)
	}
	a.SetBlocked(nil)
	got, err := p.Pick("u", "gemini", nil)
	if err != nil || got != a {
		t.Fatalf("unblocked pick = %v, %v", got, err)
	}
	a.Release()

	a.SetVerification(true, "https://accounts.google.com/x")
	if _, err := p.Pick("u", "gemini", nil); !errors.Is(err, ErrNoAccount) || !a.Status().Verify || a.Status().VerifyURL != "https://accounts.google.com/x" {
		t.Fatalf("account awaiting verification picked: %v, %+v", err, a.Status())
	}
	a.SetVerification(false, "")
	if got, err := p.Pick("u", "gemini", nil); err != nil || got != a {
		t.Fatalf("verified pick = %v, %v", got, err)
	}
	a.Release()
}

func TestPriorityAndAllowedModels(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	low, err := p.Add(AccountConfig{Name: "low", Home: filepath.Join(dir, "low")})
	if err != nil {
		t.Fatal(err)
	}
	high, err := p.Add(AccountConfig{Name: "high", Home: filepath.Join(dir, "high")})
	if err != nil {
		t.Fatal(err)
	}
	high.SetRouting(10, []string{"gemini-3.8-flash"})
	low.SetRouting(-1, nil)

	if !high.AllowsModel("gemini-3.8-flash") || high.AllowsModel("claude-opus-5-5") {
		t.Fatalf("high allowed = %v", high.AllowedModels())
	}
	if !low.AllowsModel("claude-opus-5-5") {
		t.Fatalf("low allowed = %v", low.AllowedModels())
	}

	// Higher priority wins for every user while a turn slot is free.
	for _, user := range []string{"u1", "u2", "u3", "u4"} {
		got, err := p.Pick(user, "gemini", nil)
		if err != nil || got != high {
			t.Fatalf("pick(%s) = %v, %v; want high", user, got, err)
		}
		got.Release()
	}

	// With high's slot full, Pick moves to low.
	first, err := p.Pick("u1", "gemini", nil)
	if err != nil || first != high {
		t.Fatalf("first pick = %v, %v", first, err)
	}
	second, err := p.Pick("u1", "gemini", nil)
	if err != nil || second != low {
		t.Fatalf("second pick with high busy = %v, %v; want low", second, err)
	}
	second.Release()
	first.Release()
}

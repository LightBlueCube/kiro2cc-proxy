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
	if got, err := p.Pick("u", "gemini", nil); err != nil || got != a {
		t.Fatalf("unblocked pick = %v, %v", got, err)
	}
}

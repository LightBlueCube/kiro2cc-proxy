package pool

import (
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
)

func TestProxyEnv(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	extra := []string{"X=1", "HTTPS_PROXY=http://ignored:1"}
	a, err := p.Add(AccountConfig{Name: "a", Home: filepath.Join(t.TempDir(), "a"), Env: extra, Proxy: "http://127.0.0.1:7890"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(extra), "HTTPS_PROXY=http://127.0.0.1:7890", "HTTP_PROXY=http://127.0.0.1:7890")
	if got := a.Env(); !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}
	a.SetProxy("")
	if got := a.Env(); !slices.Equal(got, extra) {
		t.Fatalf("env without proxy = %q", got)
	}
}

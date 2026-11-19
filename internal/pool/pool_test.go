package pool

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agyrelay/internal/agy"
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
	if _, err := p.Pick("u", "gemini-3.8-flash", nil, nil); err == nil || a.Status().Blocked != "no route" {
		t.Fatalf("blocked account picked: %v", err)
	}
	a.SetBlocked(nil)
	got, err := p.Pick("u", "gemini-3.8-flash", nil, nil)
	if err != nil || got != a {
		t.Fatalf("unblocked pick = %v, %v", got, err)
	}
	a.Release()

	a.SetVerification(true, "https://accounts.google.com/x")
	if _, err := p.Pick("u", "gemini-3.8-flash", nil, nil); !errors.Is(err, ErrNoAccount) || !a.Status().Verify || a.Status().VerifyURL != "https://accounts.google.com/x" {
		t.Fatalf("account awaiting verification picked: %v, %+v", err, a.Status())
	}
	a.SetVerification(false, "")
	if got, err := p.Pick("u", "gemini-3.8-flash", nil, nil); err != nil || got != a {
		t.Fatalf("verified pick = %v, %v", got, err)
	}
	a.Release()

	a.Disable(errors.New("sign-in required"))
	if _, err := p.Pick("u", "gemini-3.8-flash", nil, nil); !errors.Is(err, ErrNoAccount) || a.Status().Disabled == "" {
		t.Fatalf("disabled account picked: %v, %+v", err, a.Status())
	}
	a.Enable()
	if got, err := p.Pick("u", "gemini-3.8-flash", nil, nil); err != nil || got != a || a.Status().Disabled != "" {
		t.Fatalf("enabled pick = %v, %v, %+v", got, err, a.Status())
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
		got, err := p.Pick(user, "gemini-3.8-flash", nil, nil)
		if err != nil || got != high {
			t.Fatalf("pick(%s) = %v, %v; want high", user, got, err)
		}
		got.Release()
	}

	// With high's slot full, Pick moves to low.
	first, err := p.Pick("u1", "gemini-3.8-flash", nil, nil)
	if err != nil || first != high {
		t.Fatalf("first pick = %v, %v", first, err)
	}
	second, err := p.Pick("u1", "gemini-3.8-flash", nil, nil)
	if err != nil || second != low {
		t.Fatalf("second pick with high busy = %v, %v; want low", second, err)
	}
	second.Release()
	first.Release()
}

func TestKeyGroup(t *testing.T) {
	for model, want := range map[string]string{
		"gemini-4-argon":    GroupArgon,
		"gemini-4-carbon":   GroupArgon,
		"gemini-3.8-flash":  GroupGemini,
		"gemini-4-hydrogen": GroupGemini,
		"gemini-4-neon":     GroupGemini,
		"claude-sonnet-5-5": GroupClaudeGPT,
		"gpt-oss-120b":      GroupClaudeGPT,
	} {
		if got := KeyGroup(model); got != want {
			t.Errorf("KeyGroup(%s) = %s, want %s", model, got, want)
		}
	}
}

func newPoolGroupAccount(t *testing.T, p *Pool, name string, models []string, quotaGroups []string) *Account {
	t.Helper()
	a, err := p.Add(AccountConfig{Name: name, Home: filepath.Join(t.TempDir(), name)})
	if err != nil {
		t.Fatal(err)
	}
	var catalog []agy.ModelInfo
	for _, id := range models {
		catalog = append(catalog, agy.ModelInfo{ModelID: id})
	}
	a.SetModels(catalog)
	if quotaGroups != nil {
		a.SetQuotaGroups(quotaGroups)
	}
	return a
}

func TestPoolGroup(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	proModels := []string{"gemini-4-argon-medium", "gemini-3.8-flash-medium"}
	ultraModels := append(slices.Clone(proModels), "gemini-4-carbon-medium")
	proBuckets := []string{GroupGemini, GroupClaudeGPT, GroupArgon}
	ultraBuckets := []string{GroupGemini, GroupClaudeGPT}

	cases := []struct {
		name    string
		account *Account
		want    map[string]string
	}{
		{"pro", newPoolGroupAccount(t, p, "pro", proModels, proBuckets), map[string]string{
			"gemini-4-argon": GroupArgon, "gemini-4-carbon": GroupArgon, "gemini-3.8-flash": GroupGemini, "gemini-4-neon": GroupGemini, "claude-opus-5-5": GroupClaudeGPT}},
		{"ultra", newPoolGroupAccount(t, p, "ultra", ultraModels, ultraBuckets), map[string]string{
			"gemini-4-argon": GroupGemini, "gemini-4-carbon": GroupGemini, "gemini-3.8-flash": GroupGemini, "gemini-4-neon": GroupGemini, "claude-opus-5-5": GroupClaudeGPT}},
		{"carbon offered, not sampled", newPoolGroupAccount(t, p, "unsampled", ultraModels, nil), map[string]string{
			"gemini-4-argon": GroupArgon, "gemini-4-carbon": GroupArgon}},
		{"sampled without Argon bucket, no carbon", newPoolGroupAccount(t, p, "free", proModels, ultraBuckets), map[string]string{
			"gemini-4-argon": GroupArgon}},
		{"carbon offered with an Argon bucket", newPoolGroupAccount(t, p, "both", ultraModels, proBuckets), map[string]string{
			"gemini-4-argon": GroupArgon, "gemini-4-carbon": GroupArgon}},
	}
	for _, c := range cases {
		for model, want := range c.want {
			if got := c.account.PoolGroup(model); got != want {
				t.Errorf("%s: PoolGroup(%s) = %s, want %s", c.name, model, got, want)
			}
		}
	}
}

// An exhausted Argon bucket cools the Argon group; an account that charges
// Argon to the Gemini bucket stays pickable for Argon models.
func TestPickByAccountPoolGroup(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	pro := newPoolGroupAccount(t, p, "pro", []string{"gemini-4-argon-medium"}, []string{GroupGemini, GroupClaudeGPT, GroupArgon})
	ultra := newPoolGroupAccount(t, p, "ultra", []string{"gemini-4-argon-medium", "gemini-4-carbon-medium"}, []string{GroupGemini, GroupClaudeGPT})
	until := time.Now().Add(time.Hour)
	pro.Cool(GroupArgon, until)
	ultra.Cool(GroupArgon, until)

	got, err := p.Pick("u", "gemini-4-argon", nil, nil)
	if err != nil || got != ultra {
		t.Fatalf("pick with the Argon group cooling = %v, %v; want ultra", got, err)
	}
	got.Release()

	ultra.Cool(GroupGemini, until)
	if _, err := p.Pick("u", "gemini-4-argon", nil, nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("pick with the Gemini group cooling on ultra and Argon on pro: %v", err)
	}
}

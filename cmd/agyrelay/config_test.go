package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agyrelay/internal/guard"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agyrelay.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTrustedProxies(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"data_dir": "d", "agy_bin": "a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.proxies, guard.LoopbackProxies) {
		t.Errorf("default proxies = %v, want loopback", cfg.proxies)
	}

	cfg, err = loadConfig(writeConfig(t, `{"data_dir": "d", "agy_bin": "a", "trusted_proxies": ["172.17.0.1/16", "fd00::/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.proxies; len(got) != 2 || got[0].String() != "172.17.0.0/16" || got[1].String() != "fd00::/8" {
		t.Errorf("proxies = %v", got)
	}

	cfg, err = loadConfig(writeConfig(t, `{"data_dir": "d", "agy_bin": "a", "trusted_proxies": []}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.proxies) != 0 {
		t.Errorf("empty list should trust no proxy, got %v", cfg.proxies)
	}

	if _, err := loadConfig(writeConfig(t, `{"data_dir": "d", "agy_bin": "a", "trusted_proxies": ["172.17.0.1"]}`)); err == nil {
		t.Error("a bare address was accepted; want a CIDR prefix")
	}
}

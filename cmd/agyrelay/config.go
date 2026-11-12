package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"

	"agyrelay/internal/guard"
)

// config is the startup configuration file. Accounts, API keys, and
// settings live in the database and are managed in the admin UI.
//
//	{
//	  "listen": "127.0.0.1:8787",
//	  "data_dir": "./data",
//	  "agy_bin": "/opt/agy-1.5.3/antigravity",
//	  "trusted_proxies": ["127.0.0.0/8", "::1/128"]
//	}
//
// trusted_proxies are the networks of the reverse proxies or tunnels in
// front, whose forwarding headers name the client; the default is
// loopback.
type config struct {
	Listen         string   `json:"listen"`
	DataDir        string   `json:"data_dir"`
	AgyBin         string   `json:"agy_bin"`
	TrustedProxies []string `json:"trusted_proxies"`

	proxies guard.Proxies
}

const defaultListen = "127.0.0.1:8787"

// loadConfig reads and validates the file. Relative paths resolve against
// the file's directory.
func loadConfig(path string) (*config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Absolute, because agy runs in account directories and a bare name
	// would be looked up in PATH.
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if cfg.DataDir == "" || cfg.AgyBin == "" {
		return nil, errors.New("data_dir and agy_bin are required")
	}
	cfg.DataDir = resolve(base, cfg.DataDir)
	cfg.AgyBin = resolve(base, cfg.AgyBin)
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	cfg.proxies = guard.LoopbackProxies
	if cfg.TrustedProxies != nil {
		cfg.proxies = make(guard.Proxies, 0, len(cfg.TrustedProxies))
		for _, s := range cfg.TrustedProxies {
			prefix, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("%s: trusted_proxies: %w", path, err)
			}
			cfg.proxies = append(cfg.proxies, prefix.Masked())
		}
	}
	return &cfg, nil
}

// loopbackOnly reports whether the listen address accepts local
// connections only.
func (c *config) loopbackOnly() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func (c *config) dbPath() string { return filepath.Join(c.DataDir, "agyrelay.db") }

func (c *config) accountsDir() string { return filepath.Join(c.DataDir, "accounts") }

func (c *config) derivedAgyDir() string { return filepath.Join(c.DataDir, "agy-derived") }

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

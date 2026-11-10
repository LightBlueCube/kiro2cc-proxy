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
)

// config is the startup configuration file. Accounts, API keys, and
// settings live in the database and are managed in the admin UI.
//
//	{
//	  "listen": "127.0.0.1:8787",
//	  "data_dir": "./data",
//	  "agy_bin": "/opt/agy-1.5.3/antigravity"
//	}
type config struct {
	Listen            string `json:"listen"`
	DataDir           string `json:"data_dir"`
	AgyBin            string `json:"agy_bin"`
	ProcessPerSession *bool  `json:"process_per_session"`
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
	if cfg.ProcessPerSession == nil {
		enabled := true
		cfg.ProcessPerSession = &enabled
	}
	return &cfg, nil
}

// loopbackOnly reports whether the listen address accepts local
// connections only. The admin UI needs it: without TLS of its own, it is
// reachable from outside only through a tunnel or proxy that adds TLS.
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

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agyrelay/internal/server"
)

// config is the JSON configuration file.
//
//	{
//	  "listen": "127.0.0.1:8787",
//	  "data_dir": "./data",
//	  "agy_bin": "/opt/agy-1.5.3/antigravity",
//	  "accounts": [{"name": "main"}, {"name": "spare", "env": ["HTTPS_PROXY=http://127.0.0.1:3128"]}],
//	  "keys": [{"name": "alice", "sha256": "<from agyrelay keygen>"}]
//	}
type config struct {
	Listen            string          `json:"listen"`
	DataDir           string          `json:"data_dir"`
	AgyBin            string          `json:"agy_bin"`
	TurnsPerAccount   int             `json:"turns_per_account"`
	ProcessPerSession *bool           `json:"process_per_session"`
	SnapshotTTL       string          `json:"snapshot_ttl"`
	Accounts          []accountConfig `json:"accounts"`
	Keys              []keyConfig     `json:"keys"`

	snapshotTTL time.Duration
	keys        []server.Key
}

type accountConfig struct {
	Name string   `json:"name"`
	Env  []string `json:"env"`
}

type keyConfig struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

const (
	defaultListen          = "127.0.0.1:8787"
	defaultTurnsPerAccount = 8
	defaultSnapshotTTL     = 24 * time.Hour
)

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
	base := filepath.Dir(path)
	if cfg.DataDir == "" || cfg.AgyBin == "" {
		return nil, errors.New("data_dir and agy_bin are required")
	}
	cfg.DataDir = resolve(base, cfg.DataDir)
	cfg.AgyBin = resolve(base, cfg.AgyBin)
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	if cfg.TurnsPerAccount == 0 {
		cfg.TurnsPerAccount = defaultTurnsPerAccount
	}
	if cfg.ProcessPerSession == nil {
		enabled := true
		cfg.ProcessPerSession = &enabled
	}
	cfg.snapshotTTL = defaultSnapshotTTL
	if cfg.SnapshotTTL != "" {
		if cfg.snapshotTTL, err = time.ParseDuration(cfg.SnapshotTTL); err != nil {
			return nil, fmt.Errorf("snapshot_ttl: %w", err)
		}
	}
	for _, key := range cfg.Keys {
		sum, err := hex.DecodeString(key.SHA256)
		if err != nil || len(sum) != 32 || key.Name == "" {
			return nil, fmt.Errorf("key %q: need a name and a 64-digit hex sha256", key.Name)
		}
		cfg.keys = append(cfg.keys, server.Key{Name: key.Name, SHA256: [32]byte(sum)})
	}
	if len(cfg.keys) == 0 {
		return nil, errors.New("no API keys configured")
	}
	return &cfg, nil
}

// accountHome is the HOME agy runs with for an account.
func (c *config) accountHome(name string) string {
	return filepath.Join(c.DataDir, "accounts", name)
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

// Package control is agyrelay's control plane: it loads settings,
// accounts, and API keys from the store, applies them to the running
// components, and changes them on the admin's behalf.
package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

const settingsKey = "settings"

// Settings are the admin-editable knobs.
type Settings struct {
	TurnsPerAccount int `json:"turns_per_account"`
	// SnapshotTTL is a Go duration ("24h"): how long an idle session stays
	// reusable.
	SnapshotTTL string `json:"snapshot_ttl"`
	// LogLevel is debug, info, warn, or error.
	LogLevel string `json:"log_level"`
}

func defaultSettings() Settings {
	return Settings{TurnsPerAccount: 8, SnapshotTTL: "24h", LogLevel: "info"}
}

var logLevels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

func (s Settings) validate() error {
	if s.TurnsPerAccount < 1 || s.TurnsPerAccount > 64 {
		return errors.New("turns_per_account must be between 1 and 64")
	}
	ttl, err := time.ParseDuration(s.SnapshotTTL)
	if err != nil || ttl < time.Minute {
		return errors.New("snapshot_ttl must be a duration of at least 1m, e.g. 24h")
	}
	if _, ok := logLevels[s.LogLevel]; !ok {
		return errors.New("log_level must be debug, info, warn, or error")
	}
	return nil
}

// SnapshotTTLDuration is SnapshotTTL parsed; settings are validated on load
// and update.
func (s Settings) SnapshotTTLDuration() time.Duration {
	ttl, _ := time.ParseDuration(s.SnapshotTTL)
	return ttl
}

// LoadSettings reads the stored settings over the defaults. The process
// uses them to build the pool and gateway before New.
func LoadSettings(ctx context.Context, st *store.Store) (Settings, error) {
	s := defaultSettings()
	if _, err := st.Setting(ctx, settingsKey, &s); err != nil {
		return Settings{}, err
	}
	if err := s.validate(); err != nil {
		return Settings{}, fmt.Errorf("stored settings: %w", err)
	}
	return s, nil
}

// Level is LogLevel as a slog level.
func (s Settings) Level() slog.Level { return logLevels[s.LogLevel] }

type Config struct {
	Store   *store.Store
	Pool    *pool.Pool
	Gateway *gateway.Gateway
	Monitor *usage.Monitor
	// AgyBin runs sign-ins.
	AgyBin string
	// Level is the process log level, changed with the setting.
	Level *slog.LevelVar
	// AccountsDir holds one HOME directory per account.
	AccountsDir string
	Settings    Settings
	Log         *slog.Logger
}

type Control struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	settings Settings
	keys     map[[sha256.Size]byte]conv.Caller
	logins   map[string]*pendingLogin
}

// New registers the stored accounts with the pool and loads the API keys.
func New(ctx context.Context, cfg Config) (*Control, error) {
	c := &Control{cfg: cfg, log: cfg.Log, settings: cfg.Settings, logins: map[string]*pendingLogin{}}
	accounts, err := cfg.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		account, err := cfg.Pool.Add(pool.AccountConfig{Name: a.Name, Home: c.accountHome(a.Name), Env: a.Env})
		if err != nil {
			return nil, err
		}
		account.SetPaused(a.Paused)
	}
	if err := c.reloadKeys(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Control) accountHome(name string) string { return filepath.Join(c.cfg.AccountsDir, name) }

func (c *Control) Settings() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

// UpdateSettings validates, stores, and applies s.
func (c *Control) UpdateSettings(ctx context.Context, s Settings) error {
	if err := s.validate(); err != nil {
		return &InputError{err.Error()}
	}
	if err := c.cfg.Store.SetSetting(ctx, settingsKey, s); err != nil {
		return err
	}
	c.mu.Lock()
	c.settings = s
	c.mu.Unlock()
	c.cfg.Pool.SetTurnsPerAccount(s.TurnsPerAccount)
	c.cfg.Gateway.SetSnapshotTTL(s.SnapshotTTLDuration())
	c.cfg.Level.Set(s.Level())
	return nil
}

// InputError is a request the admin can fix; its message is shown as is.
type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }

// AddAccount registers a new account; it needs a sign-in before use.
func (c *Control) AddAccount(ctx context.Context, name string, env []string) (*pool.Account, error) {
	if !pool.ValidAccountName(name) {
		return nil, &InputError{"account names use letters, digits, '.', '_', '-' and start with a letter or digit"}
	}
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return nil, &InputError{fmt.Sprintf("environment entry %q is not NAME=value", kv)}
		}
	}
	err := c.cfg.Store.AddAccount(ctx, store.Account{Name: name, Env: env, CreatedAt: time.Now()})
	if errors.Is(err, store.ErrExists) {
		return nil, &InputError{fmt.Sprintf("account %q exists", name)}
	}
	if err != nil {
		return nil, err
	}
	account, err := c.cfg.Pool.Add(pool.AccountConfig{Name: name, Home: c.accountHome(name), Env: env})
	if err != nil {
		if rmErr := c.cfg.Store.RemoveAccount(ctx, name); rmErr != nil {
			c.log.Error("account half-added", "account", name, "err", rmErr)
		}
		return nil, err
	}
	return account, nil
}

func (c *Control) SetAccountPaused(ctx context.Context, name string, paused bool) error {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return store.ErrNotFound
	}
	if err := c.cfg.Store.SetAccountPaused(ctx, name, paused); err != nil {
		return err
	}
	account.SetPaused(paused)
	return nil
}

// RemoveAccount unregisters the account; its HOME directory stays.
func (c *Control) RemoveAccount(ctx context.Context, name string) error {
	if err := c.cfg.Store.RemoveAccount(ctx, name); err != nil {
		return err
	}
	c.CancelLogin(name)
	c.cfg.Monitor.Forget(name)
	return c.cfg.Pool.Remove(name)
}

// AccountRecords returns the stored side of the accounts (env, creation
// time) by name.
func (c *Control) AccountRecords(ctx context.Context) (map[string]store.Account, error) {
	accounts, err := c.cfg.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.Account, len(accounts))
	for _, a := range accounts {
		out[a.Name] = a
	}
	return out, nil
}

// Lookup resolves an API key to its caller.
func (c *Control) Lookup(token string) (conv.Caller, bool) {
	if token == "" {
		return conv.Caller{}, false
	}
	sum := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	caller, ok := c.keys[sum]
	return caller, ok
}

func (c *Control) Keys(ctx context.Context) ([]store.Key, error) { return c.cfg.Store.Keys(ctx) }

// Models is the gateway's model catalog.
func (c *Control) Models() []conv.ModelInfo { return c.cfg.Gateway.Models() }

const keyPrefix = "sk-agyrelay-"

// CreateKey stores a new key and returns its plaintext, which is not kept.
func (c *Control) CreateKey(ctx context.Context, name, note string, models []string) (string, error) {
	name = strings.TrimSpace(name)
	if err := validateKeyName(name); err != nil {
		return "", err
	}
	token := keyPrefix + rand.Text()
	_, err := c.cfg.Store.AddKey(ctx, store.Key{Name: name, SHA256: sha256.Sum256([]byte(token)), Note: note, Models: cleanModels(models), CreatedAt: time.Now()})
	if errors.Is(err, store.ErrExists) {
		return "", &InputError{fmt.Sprintf("key name %q is taken (revoked keys keep their name)", name)}
	}
	if err != nil {
		return "", err
	}
	return token, c.reloadKeys(ctx)
}

func (c *Control) UpdateKey(ctx context.Context, id int64, note string, models []string) error {
	if err := c.cfg.Store.UpdateKey(ctx, id, note, cleanModels(models)); err != nil {
		return err
	}
	return c.reloadKeys(ctx)
}

func (c *Control) RevokeKey(ctx context.Context, id int64) error {
	if err := c.cfg.Store.RevokeKey(ctx, id, time.Now()); err != nil {
		return err
	}
	return c.reloadKeys(ctx)
}

func (c *Control) reloadKeys(ctx context.Context) error {
	keys, err := c.cfg.Store.Keys(ctx)
	if err != nil {
		return err
	}
	byHash := make(map[[sha256.Size]byte]conv.Caller, len(keys))
	for _, k := range keys {
		if k.RevokedAt.IsZero() {
			byHash[k.SHA256] = conv.Caller{Name: k.Name, Models: k.Models}
		}
	}
	c.mu.Lock()
	c.keys = byHash
	c.mu.Unlock()
	return nil
}

func validateKeyName(name string) error {
	if name == "" || len(name) > 64 {
		return &InputError{"key names have 1 to 64 characters"}
	}
	if strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == '/' }) {
		return &InputError{"key names cannot contain control characters or '/'"}
	}
	return nil
}

func cleanModels(models []string) []string {
	var out []string
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

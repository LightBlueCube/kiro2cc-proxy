// Package control is agyrelay's control plane: it loads settings,
// accounts, and API keys from the store, applies them to the running
// components, and changes them on the admin's behalf.
package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"agyrelay/internal/agy"
	"agyrelay/internal/cloudcode"
	"agyrelay/internal/conv"
	"agyrelay/internal/egress"
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
	// SessionIdleClose is a Go duration ("5m"): how long a session stays
	// open in agy without a turn. Closing it frees its worker (~250 MiB);
	// the conversation's next request reopens it in a few seconds.
	SessionIdleClose string `json:"session_idle_close"`
	// LogLevel is debug, info, warn, or error.
	LogLevel string `json:"log_level"`
	// RetentionDays bounds how long request records, quota samples, and
	// audit entries are kept.
	RetentionDays int `json:"retention_days"`
	// Prices are the list prices that estimate each request's API cost.
	Prices []usage.Price `json:"prices"`
	// ContextCheck turns on the check of each prompt's estimate against
	// ContextWindows; it costs the tokenizers' memory and counting time.
	ContextCheck   bool                    `json:"context_check"`
	ContextWindows []gateway.ContextWindow `json:"context_windows"`
	// FallbackProxy carries the traffic of accounts without their own
	// proxy ("" = none).
	FallbackProxy string `json:"fallback_proxy"`
	// AllowDirect lets accounts with no proxy at all connect directly
	// (a server abroad); otherwise their traffic is refused.
	AllowDirect bool `json:"allow_direct"`
}

// DefaultSettings are the settings before the admin changes any.
func DefaultSettings() Settings {
	return Settings{TurnsPerAccount: 8, SnapshotTTL: "24h", SessionIdleClose: "5m", LogLevel: "info", RetentionDays: 90,
		Prices: slices.Clone(usage.DefaultPrices), ContextWindows: slices.Clone(gateway.DefaultContextWindows)}
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
	idle, err := time.ParseDuration(s.SessionIdleClose)
	if err != nil || idle < time.Minute {
		return errors.New("session_idle_close must be a duration of at least 1m, e.g. 5m")
	}
	if _, ok := logLevels[s.LogLevel]; !ok {
		return errors.New("log_level must be debug, info, warn, or error")
	}
	if s.RetentionDays < 1 || s.RetentionDays > 3650 {
		return errors.New("retention_days must be between 1 and 3650")
	}
	if clean, err := cleanProxy(s.FallbackProxy); err != nil || clean != s.FallbackProxy {
		return errors.New("fallback_proxy must be empty or an http:// URL with a host and port, e.g. http://127.0.0.1:7890")
	}
	if err := gateway.ValidateContextWindows(s.ContextWindows); err != nil {
		return err
	}
	return usage.ValidatePrices(s.Prices)
}

// SnapshotTTLDuration is SnapshotTTL parsed; settings are validated on load
// and update.
func (s Settings) SnapshotTTLDuration() time.Duration {
	ttl, _ := time.ParseDuration(s.SnapshotTTL)
	return ttl
}

// SessionIdleCloseDuration is SessionIdleClose parsed (validated like
// SnapshotTTL).
func (s Settings) SessionIdleCloseDuration() time.Duration {
	idle, _ := time.ParseDuration(s.SessionIdleClose)
	return idle
}

// LoadSettings reads the stored settings over the defaults. The process
// uses them to build the pool and gateway before New.
func LoadSettings(ctx context.Context, st *store.Store) (Settings, error) {
	s := DefaultSettings()
	var raw json.RawMessage
	found, err := st.Setting(ctx, settingsKey, &raw)
	if err != nil {
		return Settings{}, err
	}
	if found {
		if err := decodeSettings(raw, &s); err != nil {
			return Settings{}, fmt.Errorf("stored settings: %w", err)
		}
	}
	if err := s.validate(); err != nil {
		return Settings{}, fmt.Errorf("stored settings: %w", err)
	}
	return s, nil
}

// decodeSettings reads stored settings over s. Stored lists replace the
// defaults whole: json would decode them into the default rows in place,
// so a row would keep the fields it lacks from whichever default sat at
// its index. A price row saved before cache_write existed takes its
// model's default cache-write price.
func decodeSettings(raw []byte, s *Settings) error {
	var stored struct {
		Prices         []map[string]json.RawMessage `json:"prices"`
		ContextWindows json.RawMessage              `json:"context_windows"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return err
	}
	if stored.Prices != nil {
		s.Prices = nil
	}
	if stored.ContextWindows != nil {
		s.ContextWindows = nil
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return err
	}

	for i, row := range stored.Prices {
		if _, saved := row["cache_write"]; saved {
			continue
		}
		model := s.Prices[i].Model
		if d := slices.IndexFunc(usage.DefaultPrices, func(p usage.Price) bool { return p.Model == model }); d >= 0 {
			s.Prices[i].CacheWrite = usage.DefaultPrices[d].CacheWrite
		}
	}
	return nil
}

// Level is LogLevel as a slog level.
func (s Settings) Level() slog.Level { return logLevels[s.LogLevel] }

type Config struct {
	Store    *store.Store
	Pool     *pool.Pool
	Gateway  *gateway.Gateway
	Monitor  *usage.Monitor
	Recorder *usage.Recorder
	Budget   *usage.Budget
	// Egress carries every account's agy traffic.
	Egress *egress.Server
	// CloudCode carries every account's Cloud Code API calls, through
	// Egress.
	CloudCode *cloudcode.Server
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
	keys     map[string]conv.Caller // by key hash
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
		account, err := c.addToPool(a.Name, a.Env, a.Proxy)
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
	clean, err := cleanProxy(s.FallbackProxy)
	if err != nil {
		return err
	}
	s.FallbackProxy = clean
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
	c.cfg.Gateway.SetIdleClose(s.SessionIdleCloseDuration())
	c.cfg.Level.Set(s.Level())
	c.cfg.Recorder.SetPrices(s.Prices)
	if err := c.cfg.Gateway.SetContextWindows(s.ContextWindows, s.ContextCheck); err != nil {
		return err
	}
	return c.applyRoutes(ctx)
}

// ErrNoRoute blocks an account whose traffic would have nowhere to go.
var ErrNoRoute = errors.New("no proxy: set one for the account or a fallback proxy in Settings, or allow direct connections")

// routeFor picks where an account's traffic goes: its own proxy, the
// fallback proxy, a direct connection when allowed, or nowhere.
func routeFor(accountProxy string, s Settings) egress.Route {
	switch {
	case accountProxy != "":
		return egress.Route{Proxy: accountProxy}
	case s.FallbackProxy != "":
		return egress.Route{Proxy: s.FallbackProxy}
	case s.AllowDirect:
		return egress.Route{Direct: true}
	}
	return egress.Route{}
}

// applyRoute sets the account's egress route and blocks the account when
// it has none.
func (c *Control) applyRoute(account *pool.Account, accountProxy string) {
	route := routeFor(accountProxy, c.Settings())
	c.cfg.Egress.SetRoute(account.Name(), route)
	if route.Refused() {
		account.SetBlocked(ErrNoRoute)
		return
	}
	account.SetBlocked(nil)
}

func (c *Control) applyRoutes(ctx context.Context) error {
	records, err := c.AccountRecords(ctx)
	if err != nil {
		return err
	}
	for _, account := range c.cfg.Pool.Accounts() {
		c.applyRoute(account, records[account.Name()].Proxy)
	}
	return nil
}

// pruneEvery is how often records past the retention are deleted.
const pruneEvery = 6 * time.Hour

// agyLogRetention is how long agy's own logs and crash reports stay. They
// only help debugging; the retention setting covers the relay's records.
const agyLogRetention = 7 * 24 * time.Hour

// RunRetention deletes old records and agy logs now and then every
// pruneEvery until ctx ends.
func (c *Control) RunRetention(ctx context.Context) {
	for {
		days := c.Settings().RetentionDays
		if err := c.cfg.Store.Prune(ctx, time.Now().AddDate(0, 0, -days)); err != nil && ctx.Err() == nil {
			c.log.Error("pruning old records failed", "err", err)
		}
		for _, account := range c.cfg.Pool.Accounts() {
			removed, err := agy.PruneLogs(account.GeminiDir(), time.Now().Add(-agyLogRetention))
			if err != nil {
				c.log.Warn("pruning agy logs failed", "account", account.Name(), "err", err)
			}
			if removed > 0 {
				c.log.Info("old agy logs deleted", "account", account.Name(), "count", removed)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pruneEvery):
		}
	}
}

// InputError is a request the admin can fix; its message is shown as is.
type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }

// AddAccount registers a new account; it needs a sign-in before use.
func (c *Control) AddAccount(ctx context.Context, name string, env []string, proxy string) (*pool.Account, error) {
	if !pool.ValidAccountName(name) {
		return nil, &InputError{"account names use letters, digits, '.', '_', '-' and start with a letter or digit"}
	}
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return nil, &InputError{fmt.Sprintf("environment entry %q is not NAME=value", kv)}
		}
	}
	proxy, err := cleanProxy(proxy)
	if err != nil {
		return nil, err
	}
	err = c.cfg.Store.AddAccount(ctx, store.Account{Name: name, Env: env, Proxy: proxy, CreatedAt: time.Now()})
	if errors.Is(err, store.ErrExists) {
		return nil, &InputError{fmt.Sprintf("account %q exists", name)}
	}
	if err != nil {
		return nil, err
	}
	account, err := c.addToPool(name, env, proxy)
	if err != nil {
		if rmErr := c.cfg.Store.RemoveAccount(ctx, name); rmErr != nil {
			c.log.Error("account half-added", "account", name, "err", rmErr)
		}
		return nil, err
	}
	return account, nil
}

// addToPool registers the account with the egress and Cloud Code proxies
// and the pool; its agy talks only to them, and egress routes by
// accountProxy.
func (c *Control) addToPool(name string, env []string, accountProxy string) (*pool.Account, error) {
	egressURL := c.cfg.Egress.Register(name)
	// A report after the account left the pool (a call in flight while it
	// was removed) has no account to go to.
	endpoint, err := c.cfg.CloudCode.Register(name, egressURL, func(v *cloudcode.Verification) {
		account := c.cfg.Pool.Account(name)
		if account == nil {
			return
		}
		if v == nil {
			account.SetVerification(false, "")
			return
		}
		account.SetVerification(true, v.URL)
	})
	if err != nil {
		c.cfg.Egress.Forget(name)
		return nil, err
	}
	account, err := c.cfg.Pool.Add(pool.AccountConfig{Name: name, Home: c.accountHome(name), Env: env, Proxy: egressURL, CloudCodeEndpoint: endpoint})
	if err != nil {
		c.cfg.CloudCode.Forget(name)
		c.cfg.Egress.Forget(name)
		return nil, err
	}
	c.applyRoute(account, accountProxy)
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

// SetAccountProxy changes the account's own proxy ("" = use the fallback
// route). Open connections close and agy reconnects through the new
// route; agy keeps running.
func (c *Control) SetAccountProxy(ctx context.Context, name, proxy string) error {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return store.ErrNotFound
	}
	proxy, err := cleanProxy(proxy)
	if err != nil {
		return err
	}
	if err := c.cfg.Store.SetAccountProxy(ctx, name, proxy); err != nil {
		return err
	}
	c.applyRoute(account, proxy)
	return nil
}

// cleanProxy accepts "" or an http:// proxy with a host and port, the
// kind verified with agy (e.g. mihomo's HTTP port).
func cleanProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	valid := err == nil && u.Scheme == "http" && u.Hostname() != "" && u.Port() != "" &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
	if !valid {
		return "", &InputError{"the proxy must be an http:// URL with a host and port, e.g. http://127.0.0.1:7890"}
	}
	return (&url.URL{Scheme: u.Scheme, User: u.User, Host: u.Host}).String(), nil
}

// RemoveAccount unregisters the account and deletes its HOME directory:
// its sign-in and agy's sessions go with it.
func (c *Control) RemoveAccount(ctx context.Context, name string) error {
	if err := c.cfg.Store.RemoveAccount(ctx, name); err != nil {
		return err
	}
	c.CancelLogin(name)
	c.cfg.Monitor.Forget(name)
	if err := c.cfg.Pool.Remove(name); err != nil {
		return err
	}
	c.cfg.CloudCode.Forget(name)
	c.cfg.Egress.Forget(name)
	if err := os.RemoveAll(c.accountHome(name)); err != nil {
		return fmt.Errorf("account removed, but deleting its files failed: %w", err)
	}
	return nil
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
	c.mu.Lock()
	defer c.mu.Unlock()
	caller, ok := c.keys[keyHash(token)]
	return caller, ok
}

// keyHash is the key's lowercase hex SHA-256, the same as
// `printf %s "$KEY" | sha256sum`.
func keyHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (c *Control) Keys(ctx context.Context) ([]store.Key, error) { return c.cfg.Store.Keys(ctx) }

// Models is the gateway's model catalog.
func (c *Control) Models() []conv.ModelInfo { return c.cfg.Gateway.Models() }

// AccountModels is what agy last reported the account may call.
func (c *Control) AccountModels(account string) []conv.ModelInfo {
	return c.cfg.Gateway.AccountModels(account)
}

// UpstreamModels is the account's agent models from the catalog Google
// last sent its agy; nil before agy fetched one.
func (c *Control) UpstreamModels(account string) []cloudcode.AgentModel {
	return c.cfg.CloudCode.AgentModels(account)
}

// RefreshAccount samples the account's quota, starting its agy when none
// runs. Google's earlier refusals for a missing verification are
// forgotten first, so the refresh checks again whether it still wants
// one.
func (c *Control) RefreshAccount(ctx context.Context, name string) error {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return store.ErrNotFound
	}
	c.cfg.CloudCode.ClearVerification(name)
	_, err := c.cfg.Monitor.Refresh(ctx, account)
	return err
}

// AgyLogFiles lists the account's agy logs and crash reports, newest
// first.
func (c *Control) AgyLogFiles(name string) ([]agy.LogFile, error) {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return nil, store.ErrNotFound
	}
	return agy.LogFiles(account.GeminiDir())
}

// AgyLogs returns the entries of the account's agy logs that q selects.
func (c *Control) AgyLogs(name string, q agy.LogQuery) (agy.LogResult, error) {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return agy.LogResult{}, store.ErrNotFound
	}
	result, err := agy.ReadLogs(account.GeminiDir(), q)
	switch {
	case errors.Is(err, agy.ErrNoLogFile):
		return agy.LogResult{}, &InputError{"no such log file: " + q.File}
	case errors.Is(err, agy.ErrLogLevel):
		return agy.LogResult{}, &InputError{err.Error()}
	}
	return result, err
}

const keyPrefix = "sk-agyrelay-"

// CreateKey stores a new key and returns its plaintext, which is not kept,
// and its hash.
func (c *Control) CreateKey(ctx context.Context, name string, u store.KeyUpdate) (token, hash string, err error) {
	name = strings.TrimSpace(name)
	if err := validateKeyName(name); err != nil {
		return "", "", err
	}
	if err := validateLimits(u); err != nil {
		return "", "", err
	}
	token = keyPrefix + rand.Text()
	hash = keyHash(token)
	err = c.cfg.Store.AddKey(ctx, store.Key{
		Hash: hash, Name: name, Note: u.Note, Models: cleanModels(u.Models), LimitTotal: u.LimitTotal, Limits: u.Limits, CreatedAt: time.Now(),
	})
	if errors.Is(err, store.ErrExists) {
		return "", "", &InputError{fmt.Sprintf("key name %q is taken (a revoked key keeps its name until it is deleted)", name)}
	}
	if err != nil {
		return "", "", err
	}
	return token, hash, c.reloadKeys(ctx)
}

func (c *Control) UpdateKey(ctx context.Context, hash string, u store.KeyUpdate) error {
	if err := validateLimits(u); err != nil {
		return err
	}
	u.Models = cleanModels(u.Models)
	if err := c.cfg.Store.UpdateKey(ctx, hash, u); err != nil {
		return err
	}
	return c.reloadKeys(ctx)
}

// Sessions lists the gateway's ACP sessions.
func (c *Control) Sessions() []gateway.SessionInfo { return c.cfg.Gateway.Sessions() }

func (c *Control) CancelTurn(account, sessionID string) error {
	return notFound(c.cfg.Gateway.CancelTurn(account, sessionID))
}

func (c *Control) CloseSession(ctx context.Context, account, sessionID string) error {
	return notFound(c.cfg.Gateway.CloseSession(ctx, account, sessionID))
}

// notFound turns the gateway's missing-session error into a message the
// admin sees.
func notFound(err error) error {
	if errors.Is(err, gateway.ErrNoSession) {
		return &InputError{err.Error()}
	}
	return err
}

// RestartAccount kills the account's agy processes.
func (c *Control) RestartAccount(name string) error {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return store.ErrNotFound
	}
	account.Restart()
	return nil
}

// Egress is the account's outbound traffic and route.
func (c *Control) Egress(account string) (egress.Stats, bool) { return c.cfg.Egress.Stats(account) }

// KeyTotals is every key's successful requests and tokens by model.
func (c *Control) KeyTotals(ctx context.Context) ([]store.KeyTotal, error) {
	return c.cfg.Store.KeyTotals(ctx)
}

// KeySpends is every key's list-price spend in USD by quota group.
func (c *Control) KeySpends(ctx context.Context) (map[string]map[string]float64, error) {
	return c.cfg.Budget.Spends(ctx, "")
}

// Quota is the weekly-quota use attributed to matching requests.
func (c *Control) Quota(ctx context.Context, f store.RequestFilter) (usage.QuotaReport, error) {
	return usage.Quota(ctx, c.cfg.Store, f)
}

func validateLimits(u store.KeyUpdate) error {
	if u.LimitTotal != nil && len(u.Limits) > 0 {
		return &InputError{"set either a total limit or per-group limits, not both"}
	}
	if u.LimitTotal != nil && *u.LimitTotal < 0 {
		return &InputError{"the total limit cannot be negative"}
	}
	for group, limit := range u.Limits {
		if !slices.Contains(usage.Groups, group) {
			return &InputError{fmt.Sprintf("unknown quota group %q (known: %s)", group, strings.Join(usage.Groups, ", "))}
		}
		if limit < 0 {
			return &InputError{fmt.Sprintf("the %s limit cannot be negative", group)}
		}
	}
	return nil
}

func (c *Control) RevokeKey(ctx context.Context, hash string) error {
	if err := c.cfg.Store.RevokeKey(ctx, hash, time.Now()); err != nil {
		return err
	}
	return c.reloadKeys(ctx)
}

func (c *Control) RestoreKey(ctx context.Context, hash string) error {
	if err := c.cfg.Store.RestoreKey(ctx, hash); err != nil {
		return err
	}
	return c.reloadKeys(ctx)
}

// DeleteKey deletes a revoked key and its quota use, and returns its name.
func (c *Control) DeleteKey(ctx context.Context, hash string) (string, error) {
	name, err := c.cfg.Store.DeleteKey(ctx, hash)
	if errors.Is(err, store.ErrKeyActive) {
		return "", &InputError{"revoke the key before deleting it"}
	}
	return name, err
}

func (c *Control) reloadKeys(ctx context.Context) error {
	keys, err := c.cfg.Store.Keys(ctx)
	if err != nil {
		return err
	}
	byHash := make(map[string]conv.Caller, len(keys))
	for _, k := range keys {
		if k.RevokedAt.IsZero() {
			byHash[k.Hash] = conv.Caller{KeyHash: k.Hash, Name: k.Name, Models: k.Models, LimitTotal: k.LimitTotal, Limits: k.Limits}
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

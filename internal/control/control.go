// Package control is agyrelay's control plane: it loads settings,
// accounts, and API keys from the store, applies them to the running
// components, and changes them on the admin's behalf.
package control

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"agyrelay/internal/promptflags"
	"agyrelay/internal/store"
	"agyrelay/internal/sysprompt"
	"agyrelay/internal/usage"
)

const settingsKey = "settings"

const (
	maxCaptureResponseMiB = 256
	saveUpstreamTimeout   = 30 * time.Second
)

// Settings are the admin-editable knobs.
type Settings struct {
	TurnsPerAccount int `json:"turns_per_account"`
	// MaxWorkers caps open session workers across all accounts (0 =
	// unlimited); excess idle sessions are closed first.
	MaxWorkers int `json:"max_workers"`
	// SnapshotTTL is a Go duration ("24h"): how long an idle session stays
	// reusable.
	SnapshotTTL string `json:"snapshot_ttl"`
	// SessionIdleClose is a Go duration ("5m"): how long a session stays
	// open in agy without a turn. Closing it frees its worker (~250 MiB);
	// the conversation's next request reopens it in a few seconds.
	SessionIdleClose string `json:"session_idle_close"`
	// ParkTTL is a Go duration ("5m"): how long a turn waiting for the
	// client's tool results stays parked before it is cancelled.
	ParkTTL string `json:"park_ttl"`
	// LogLevel is debug, info, warn, or error.
	LogLevel string `json:"log_level"`
	// RetentionDays bounds how long request records, quota samples, and
	// audit entries are kept.
	RetentionDays int `json:"retention_days"`
	// Prices are the list prices that estimate each request's API cost.
	Prices []usage.Price `json:"prices"`
	// RefuseHarnessPrompts refuses requests whose system prompt is still a
	// harness's default (OpenCode's, Claude Code's, ...), as
	// HarnessSignatures recognize it.
	RefuseHarnessPrompts bool                  `json:"refuse_harness_prompts"`
	HarnessSignatures    []sysprompt.Signature `json:"harness_signatures"`
	// FallbackProxy carries the traffic of accounts without their own
	// proxy ("" = none).
	FallbackProxy string `json:"fallback_proxy"`
	// AllowDirect lets accounts with no proxy at all connect directly
	// (a server abroad); otherwise their traffic is refused.
	AllowDirect bool `json:"allow_direct"`
	// EgressRules are mihomo-format routing rules evaluated top-to-bottom
	// before an account's own route.
	EgressRules []string `json:"egress_rules"`
	// CaptureUpstream records the generation calls agy sends to Google and
	// their responses, for debugging; store.UpstreamRetention bounds them.
	// Turning it off deletes them.
	CaptureUpstream bool `json:"capture_upstream"`
	// CaptureResponseMiB is the most response MiB kept per captured call.
	CaptureResponseMiB int `json:"capture_response_mib"`
	// RiskControl turns on the handling of requests Google's risk control
	// refuses: a request that draws 429s without a wait while other
	// requests go through fails with 403, and its system prompt is
	// flagged and refused. Off, such a request is retried like any 429
	// and nothing is refused; the flagged prompts stay on record.
	RiskControl bool `json:"risk_control"`
	// FlaggedPromptRetentionDays is how long a flagged prompt that is not
	// pinned stays on record after its last refusal or release.
	FlaggedPromptRetentionDays int `json:"flagged_prompt_retention_days"`
	// WarmupFullPools sends a minimal request when an in-rotation account's
	// weekly quota pool is full (100% and countdown not yet started) to start
	// its 7-day reset timer, ignoring per-account allowed_models routing.
	WarmupFullPools bool `json:"warmup_full_pools"`
	// WarmupModels optionally overrides the model used to warm up each quota
	// group (gemini, claude-gpt). Empty or absent picks the cheapest
	// offered model in that group according to Prices.
	WarmupModels map[string]string `json:"warmup_models"`
}

// DefaultSettings are the settings before the admin changes any.
func DefaultSettings() Settings {
	return Settings{TurnsPerAccount: 8, SnapshotTTL: "24h", SessionIdleClose: "5m", ParkTTL: "5m", LogLevel: "info", RetentionDays: 90, CaptureResponseMiB: 4,
		RiskControl: true, FlaggedPromptRetentionDays: 30, EgressRules: slices.Clone(egress.DefaultRules),
		Prices:            slices.Clone(usage.DefaultPrices),
		HarnessSignatures: slices.Clone(sysprompt.DefaultSignatures),
		WarmupModels:      map[string]string{}}
}

var logLevels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

const maxParkTTL = 24 * time.Hour

func (s Settings) validate() error {
	if s.TurnsPerAccount < 1 || s.TurnsPerAccount > 64 {
		return errors.New("turns_per_account must be between 1 and 64")
	}
	if s.MaxWorkers < 0 || s.MaxWorkers > 1024 {
		return errors.New("max_workers must be between 0 and 1024")
	}
	ttl, err := time.ParseDuration(s.SnapshotTTL)
	if err != nil || ttl < time.Minute {
		return errors.New("snapshot_ttl must be a duration of at least 1m, e.g. 24h")
	}
	idle, err := time.ParseDuration(s.SessionIdleClose)
	if err != nil || idle < time.Minute {
		return errors.New("session_idle_close must be a duration of at least 1m, e.g. 5m")
	}
	park, err := time.ParseDuration(s.ParkTTL)
	if err != nil || park < time.Minute || park > maxParkTTL {
		return errors.New("park_ttl must be a duration between 1m and 24h, e.g. 5m")
	}
	if _, ok := logLevels[s.LogLevel]; !ok {
		return errors.New("log_level must be debug, info, warn, or error")
	}
	if s.RetentionDays < 1 || s.RetentionDays > 3650 {
		return errors.New("retention_days must be between 1 and 3650")
	}
	if s.FlaggedPromptRetentionDays < 1 || s.FlaggedPromptRetentionDays > 3650 {
		return errors.New("flagged_prompt_retention_days must be between 1 and 3650")
	}
	if s.CaptureResponseMiB < 1 || s.CaptureResponseMiB > maxCaptureResponseMiB {
		return fmt.Errorf("capture_response_mib must be between 1 and %d", maxCaptureResponseMiB)
	}
	if clean, err := cleanProxy(s.FallbackProxy); err != nil || clean != s.FallbackProxy {
		return errors.New("fallback_proxy must be empty or an http:// URL with a host and port, e.g. http://127.0.0.1:7890")
	}
	if _, err := egress.ParseRules(s.EgressRules); err != nil {
		return err
	}
	if err := sysprompt.ValidateSignatures(s.HarnessSignatures); err != nil {
		return err
	}
	if err := validateWarmupModels(s.WarmupModels); err != nil {
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

// ParkTTLDuration is ParkTTL parsed (validated like SnapshotTTL).
func (s Settings) ParkTTLDuration() time.Duration {
	park, _ := time.ParseDuration(s.ParkTTL)
	return park
}

// LoadSettings reads the stored settings over the defaults and persists
// any missing keys so new settings are initialized in the database on startup.
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
	encoded, err := json.Marshal(s)
	if err != nil {
		return Settings{}, err
	}
	if !found || !bytes.Equal(raw, encoded) {
		if err := st.SetSetting(ctx, settingsKey, s); err != nil {
			return Settings{}, err
		}
	}
	return s, nil
}

// decodeSettings reads stored settings over s. Stored lists replace the
// defaults whole: json would decode them into the default rows in place,
// so a row would keep the fields it lacks from whichever default sat at
// its index. A price row saved before cache_write existed takes its
// model's default cache-write price.
func decodeSettings(raw []byte, s *Settings) error {
	defaults := *s
	var stored struct {
		Prices            []map[string]json.RawMessage `json:"prices"`
		HarnessSignatures json.RawMessage              `json:"harness_signatures"`
		EgressRules       json.RawMessage              `json:"egress_rules"`
		WarmupModels      json.RawMessage              `json:"warmup_models"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return err
	}
	if stored.Prices != nil {
		s.Prices = nil
	}
	if stored.HarnessSignatures != nil {
		s.HarnessSignatures = nil
	}
	hasEgressRules := stored.EgressRules != nil && !bytes.Equal(bytes.TrimSpace(stored.EgressRules), []byte("null"))
	if hasEgressRules {
		s.EgressRules = []string{}
	}
	if stored.WarmupModels != nil {
		s.WarmupModels = map[string]string{}
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return err
	}
	if !hasEgressRules {
		s.EgressRules = defaults.EgressRules
	}
	s.WarmupModels = cleanWarmupModels(s.WarmupModels)
	dropLegacyArgonSettings(s)
	if s.TurnsPerAccount == 0 {
		s.TurnsPerAccount = defaults.TurnsPerAccount
	}
	if s.SnapshotTTL == "" {
		s.SnapshotTTL = defaults.SnapshotTTL
	}
	if s.SessionIdleClose == "" {
		s.SessionIdleClose = defaults.SessionIdleClose
	}
	if s.ParkTTL == "" {
		s.ParkTTL = defaults.ParkTTL
	}
	if s.LogLevel == "" {
		s.LogLevel = defaults.LogLevel
	}
	if s.RetentionDays == 0 {
		s.RetentionDays = defaults.RetentionDays
	}
	if s.CaptureResponseMiB == 0 {
		s.CaptureResponseMiB = defaults.CaptureResponseMiB
	}
	if s.FlaggedPromptRetentionDays == 0 {
		s.FlaggedPromptRetentionDays = defaults.FlaggedPromptRetentionDays
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
	// Flags holds the system prompts Google's risk control refused; the
	// gateway shares it.
	Flags *promptflags.Flags
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
	keys     map[string]conv.Caller // active keys, by key hash
	allKeys  map[string]store.Key   // active + revoked keys, by key hash
	logins   map[string]*pendingLogin
	warmup   *warmupTracker
}

// New registers the stored accounts with the pool and loads the API keys.
func New(ctx context.Context, cfg Config) (*Control, error) {
	cfg.Settings.EgressRules = cleanRuleLines(cfg.Settings.EgressRules)
	cfg.Settings.WarmupModels = cleanWarmupModels(cfg.Settings.WarmupModels)
	rules, err := egress.ParseRules(cfg.Settings.EgressRules)
	if err != nil {
		return nil, err
	}
	cfg.Egress.SetRules(rules)
	c := &Control{cfg: cfg, log: cfg.Log, settings: cfg.Settings, logins: map[string]*pendingLogin{}, warmup: newWarmupTracker()}
	accounts, err := cfg.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	nameToID := make(map[string]string, len(accounts))
	for _, a := range accounts {
		nameToID[a.Name] = a.ID
		if err := c.migrateAccountDir(a); err != nil {
			return nil, err
		}
		account, err := c.addToPool(a.ID, a.Name, a.Env, a.Proxy)
		if err != nil {
			return nil, err
		}
		if a.Paused && a.PauseReason != "" {
			account.PauseWithReason(a.PauseReason)
		} else {
			account.SetPaused(a.Paused)
		}
		account.SetRouting(a.Priority, a.Models)
	}
	if cfg.Gateway != nil {
		if err := cfg.Gateway.MigrateAccountIDs(nameToID); err != nil {
			return nil, err
		}
	}
	if err := c.reloadKeys(ctx); err != nil {
		return nil, err
	}
	cfg.Flags.SetEnabled(cfg.Settings.RiskControl)
	if err := c.applyCapture(ctx, cfg.Settings); err != nil {
		return nil, err
	}
	if cfg.Monitor != nil {
		cfg.Monitor.SetOnQuota(c.onQuotaSample)
	}
	return c, nil
}

func (c *Control) accountHome(id string) string { return filepath.Join(c.cfg.AccountsDir, id) }

// migrateAccountDir moves a pre-UUID account directory (<AccountsDir>/<name>)
// to <AccountsDir>/<id> on first startup after schema upgrade.
func (c *Control) migrateAccountDir(a store.Account) error {
	if a.ID == a.Name || !pool.ValidAccountID(a.Name) {
		return nil
	}
	newHome := c.accountHome(a.ID)
	if _, err := os.Lstat(newHome); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	oldHome := c.accountHome(a.Name)
	if _, err := os.Lstat(oldHome); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Rename(oldHome, newHome); err != nil {
		return fmt.Errorf("migrate account %s directory: %w", a.Name, err)
	}
	return nil
}

func (c *Control) Settings() Settings {
	c.mu.Lock()
	s := c.settings
	s.WarmupModels = maps.Clone(s.WarmupModels)
	c.mu.Unlock()
	if s.WarmupModels == nil {
		s.WarmupModels = map[string]string{}
	}
	return s
}

// UpdateSettings validates, stores, and applies s.
func (c *Control) UpdateSettings(ctx context.Context, s Settings) error {
	clean, err := cleanProxy(s.FallbackProxy)
	if err != nil {
		return err
	}
	s.FallbackProxy = clean
	if s.EgressRules == nil {
		s.EgressRules = c.Settings().EgressRules
	}
	s.EgressRules = cleanRuleLines(s.EgressRules)
	if s.WarmupModels == nil {
		s.WarmupModels = c.Settings().WarmupModels
	}
	s.WarmupModels = cleanWarmupModels(s.WarmupModels)
	if err := s.validate(); err != nil {
		return &InputError{err.Error()}
	}
	rules, _ := egress.ParseRules(s.EgressRules) // validated by s.validate()
	if err := c.cfg.Store.SetSetting(ctx, settingsKey, s); err != nil {
		return err
	}
	c.mu.Lock()
	c.settings = s
	c.mu.Unlock()
	c.cfg.Pool.SetTurnsPerAccount(s.TurnsPerAccount)
	c.cfg.Gateway.SetMaxWorkers(s.MaxWorkers)
	c.cfg.Gateway.SetSnapshotTTL(s.SnapshotTTLDuration())
	c.cfg.Gateway.SetIdleClose(s.SessionIdleCloseDuration())
	c.cfg.Gateway.SetParkTTL(s.ParkTTLDuration())
	c.cfg.Gateway.SetHarnessPrompts(s.HarnessSignatures, s.RefuseHarnessPrompts)
	c.cfg.Flags.SetEnabled(s.RiskControl)
	c.cfg.Level.Set(s.Level())
	c.cfg.Recorder.SetPrices(s.Prices)
	if err := c.applyCapture(ctx, s); err != nil {
		return err
	}
	c.cfg.Egress.SetRules(rules)
	if err := c.applyRoutes(ctx); err != nil {
		return err
	}
	c.wakeWarmup()
	return nil
}

func cleanRuleLines(lines []string) []string {
	out := []string{}
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// applyCapture starts or stops the recording of agy's generation calls;
// stopping deletes the recorded ones.
func (c *Control) applyCapture(ctx context.Context, s Settings) error {
	if !s.CaptureUpstream {
		c.cfg.CloudCode.SetCapture(nil)
		return c.cfg.Store.DeleteUpstreamCalls(ctx)
	}
	c.cfg.CloudCode.SetCapture(&cloudcode.Capture{
		ResponseLimit: int64(s.CaptureResponseMiB) << 20,
		Begin:         c.cfg.Gateway.CallTokenOf,
		Save:          c.saveUpstreamCall,
	})
	return nil
}

// saveUpstreamCall stores a captured call off the response path. A call
// that ended after the capture was turned off is dropped.
func (c *Control) saveUpstreamCall(call cloudcode.Captured) {
	if !c.Settings().CaptureUpstream {
		return
	}
	accountName := ""
	if account := c.cfg.Pool.Account(call.Account); account != nil {
		accountName = account.Name()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), saveUpstreamTimeout)
		defer cancel()
		err := c.cfg.Store.AddUpstreamCall(ctx, store.UpstreamCall{
			At: call.At, CallToken: call.Tag, Account: call.Account, AccountName: accountName, Session: call.Session, Seq: call.Seq, Model: call.Model,
			Status: call.Status, Request: call.Request, Response: call.Response, ResponseTruncated: call.ResponseTruncated,
		})
		if err != nil {
			c.log.Error("upstream call not saved", "account", cmp.Or(accountName, call.Account), "session", call.Session, "err", err)
		}
	}()
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
	c.cfg.Egress.SetRoute(account.ID(), route)
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
		c.applyRoute(account, records[account.ID()].Proxy)
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
		if err := c.cfg.Store.PruneUpstream(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("pruning captured upstream calls failed", "err", err)
		}
		flaggedDays := c.Settings().FlaggedPromptRetentionDays
		if err := c.cfg.Store.PruneFlaggedPrompts(ctx, time.Now().AddDate(0, 0, -flaggedDays)); err != nil && ctx.Err() == nil {
			c.log.Error("pruning flagged prompts failed", "err", err)
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

func validateAccountName(name string) error {
	if name == "" || len(name) > 64 {
		return &InputError{"account names have 1 to 64 characters"}
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return &InputError{"account names cannot contain control characters"}
	}
	return nil
}

// AddAccount registers a new account; it needs a sign-in before use.
func (c *Control) AddAccount(ctx context.Context, name string, env []string, proxy string) (*pool.Account, error) {
	name = strings.TrimSpace(name)
	if err := validateAccountName(name); err != nil {
		return nil, err
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
	id := store.NewUUID()
	err = c.cfg.Store.AddAccount(ctx, store.Account{ID: id, Name: name, Env: env, Proxy: proxy, CreatedAt: time.Now()})
	if errors.Is(err, store.ErrExists) {
		return nil, &InputError{fmt.Sprintf("account %q exists", name)}
	}
	if err != nil {
		return nil, err
	}
	account, err := c.addToPool(id, name, env, proxy)
	if err != nil {
		if rmErr := c.cfg.Store.RemoveAccount(ctx, id); rmErr != nil {
			c.log.Error("account half-added", "account", name, "err", rmErr)
		}
		return nil, err
	}
	return account, nil
}

// RenameAccount changes the account's display name without touching its
// directory, running agy processes, or open sessions.
func (c *Control) RenameAccount(ctx context.Context, id, newName string) (oldName string, err error) {
	newName = strings.TrimSpace(newName)
	if err := validateAccountName(newName); err != nil {
		return "", err
	}
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return "", store.ErrNotFound
	}
	oldName = account.Name()
	if newName == oldName {
		return oldName, nil
	}
	err = c.cfg.Store.RenameAccount(ctx, id, newName)
	if errors.Is(err, store.ErrExists) {
		return "", &InputError{fmt.Sprintf("account %q exists", newName)}
	}
	if err != nil {
		return "", err
	}
	account.SetName(newName)
	return oldName, nil
}

// addToPool registers the account with the egress and Cloud Code proxies
// and the pool; its agy talks only to them, and egress routes by
// accountProxy.
func (c *Control) addToPool(id, name string, env []string, accountProxy string) (*pool.Account, error) {
	egressURL := c.cfg.Egress.Register(id)
	// A report after the account left the pool (a call in flight while it
	// was removed) has no account to go to.
	endpoint, err := c.cfg.CloudCode.Register(id, egressURL, func(v *cloudcode.Verification) {
		account := c.cfg.Pool.Account(id)
		if account == nil {
			return
		}
		if v == nil {
			account.SetVerification(false, "")
			return
		}
		account.SetVerification(true, v.URL)
	}, func(reason string) {
		c.pauseAccountForLocation(id, reason)
	}, func(reason string) {
		c.pauseAccountForBan(id, reason)
	}, func(session, model string, callErr *cloudcode.CallError) bool {
		account := c.cfg.Pool.Account(id)
		if account == nil {
			return false
		}
		var failure *pool.ModelFailure
		if callErr != nil {
			failure = &pool.ModelFailure{Code: callErr.Code, Message: callErr.Message, Truncated: callErr.Truncated}
			if callErr.RetryAfter > 0 {
				failure.Reset = time.Now().Add(callErr.RetryAfter)
			}
		}
		// The call is recorded either way: the account's failure state
		// does not depend on risk control.
		switch account.RecordModelCall(session, model, failure) {
		case pool.CallRisky:
			if !c.cfg.Flags.Enabled() {
				return false
			}
			c.cfg.Gateway.StopRiskySession(account, session, callErr.Request, callErr.Response)
			return true
		case pool.CallStuck:
			c.cfg.Gateway.StopStuckSession(account, session, *failure)
			return true
		}
		return false
	})
	if err != nil {
		c.cfg.Egress.Forget(id)
		return nil, err
	}
	account, err := c.cfg.Pool.Add(pool.AccountConfig{ID: id, Name: name, Home: c.accountHome(id), Env: env, Proxy: egressURL, CloudCodeEndpoint: endpoint})
	if err != nil {
		c.cfg.CloudCode.Forget(id)
		c.cfg.Egress.Forget(id)
		return nil, err
	}
	c.applyRoute(account, accountProxy)
	return account, nil
}

const autoPauseTimeout = 5 * time.Second

// pauseAccountForLocation takes an account out of rotation and persists the
// reason after Google rejects its exit location. The account stays paused
// across restarts and proxy edits until an administrator manually resumes it.
func (c *Control) pauseAccountForLocation(id, reason string) {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return
	}
	if !account.PauseWithReason(reason) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoPauseTimeout)
	defer cancel()
	if err := c.cfg.Store.SetAccountPaused(ctx, id, true, reason); err != nil {
		c.log.Error("failed to persist account pause for unsupported location", "account", account.Name(), "err", err)
	}
	c.log.Error("account paused because Google rejected its location; change its proxy and resume manually", "account", account.Name(), "reason", reason)
}

// pauseAccountForBan takes an account out of rotation and persists the
// reason after Google bans it for a Terms of Service violation. The account
// stays paused across restarts until an administrator manually resumes it.
func (c *Control) pauseAccountForBan(id, reason string) {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return
	}
	if !account.PauseWithReason(reason) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoPauseTimeout)
	defer cancel()
	if err := c.cfg.Store.SetAccountPaused(ctx, id, true, reason); err != nil {
		c.log.Error("failed to persist account pause for Google ban", "account", account.Name(), "err", err)
	}
	c.log.Error("account paused because Google banned it; appeal the ban, then resume manually", "account", account.Name(), "reason", reason)
}

func (c *Control) SetAccountPaused(ctx context.Context, id string, paused bool) error {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return store.ErrNotFound
	}
	if err := c.cfg.Store.SetAccountPaused(ctx, id, paused, ""); err != nil {
		return err
	}
	account.SetPaused(paused)
	if !paused {
		c.wakeWarmup()
	}
	return nil
}

// SetAccountRouting changes the account's routing priority and the
// models it may serve (empty allows all models it offers).
func (c *Control) SetAccountRouting(ctx context.Context, id string, priority int, models []string) error {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return store.ErrNotFound
	}
	models = cleanModels(models)
	if err := c.cfg.Store.SetAccountRouting(ctx, id, priority, models); err != nil {
		return err
	}
	account.SetRouting(priority, models)
	return nil
}

// SetAccountProxy changes the account's own proxy ("" = use the fallback
// route). Open connections close and agy reconnects through the new
// route; agy keeps running.
func (c *Control) SetAccountProxy(ctx context.Context, id, proxy string) error {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return store.ErrNotFound
	}
	proxy, err := cleanProxy(proxy)
	if err != nil {
		return err
	}
	if err := c.cfg.Store.SetAccountProxy(ctx, id, proxy); err != nil {
		return err
	}
	c.applyRoute(account, proxy)
	c.wakeWarmup()
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
// its sign-in and agy's sessions go with it. It returns the removed
// account's display name.
func (c *Control) RemoveAccount(ctx context.Context, id string) (string, error) {
	name := id
	if account := c.cfg.Pool.Account(id); account != nil {
		name = account.Name()
	}
	if err := c.cfg.Store.RemoveAccount(ctx, id); err != nil {
		return "", err
	}
	c.CancelLogin(id)
	c.resetAccountWarmup(id)
	c.cfg.Monitor.Forget(id)
	if err := c.cfg.Pool.Remove(id); err != nil {
		return name, err
	}
	c.cfg.CloudCode.Forget(id)
	c.cfg.Egress.Forget(id)
	if err := os.RemoveAll(c.accountHome(id)); err != nil {
		return name, fmt.Errorf("account removed, but deleting its files failed: %w", err)
	}
	return name, nil
}

// AccountRecords returns the stored side of the accounts (env, creation
// time) by ID.
func (c *Control) AccountRecords(ctx context.Context) (map[string]store.Account, error) {
	accounts, err := c.cfg.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.Account, len(accounts))
	for _, a := range accounts {
		out[a.ID] = a
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

// LookupKey resolves a plaintext API key (active or revoked) to its stored
// record in memory.
func (c *Control) LookupKey(token string) (store.Key, bool) {
	if token == "" {
		return store.Key{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.allKeys[keyHash(token)]
	return k, ok
}

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
func (c *Control) RefreshAccount(ctx context.Context, id string) error {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return store.ErrNotFound
	}
	c.cfg.CloudCode.ClearVerification(id)
	_, err := c.cfg.Monitor.Refresh(ctx, account)
	return err
}

// AgyLogFiles lists the account's agy logs and crash reports, newest
// first.
func (c *Control) AgyLogFiles(id string) ([]agy.LogFile, error) {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return nil, store.ErrNotFound
	}
	return agy.LogFiles(account.GeminiDir())
}

// AgyLogs returns the entries of the account's agy logs that q selects.
func (c *Control) AgyLogs(id string, q agy.LogQuery) (agy.LogResult, error) {
	account := c.cfg.Pool.Account(id)
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
		Hash: hash, Name: name, Note: u.Note, Models: cleanModels(u.Models), MaxWorkers: u.MaxWorkers, LimitTotal: u.LimitTotal, Limits: u.Limits, CreatedAt: time.Now(),
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
	if err := c.reloadKeys(ctx); err != nil {
		return err
	}
	c.cfg.Gateway.TrimKeyIdle(hash, u.MaxWorkers)
	return nil
}

// ResetKeyUsage clears a key's cumulative spend (key_usage), across every
// quota group when group is empty or for one group. With includeStats, it also
// clears key_quota and key_totals for the same scope.
func (c *Control) ResetKeyUsage(ctx context.Context, hash, group string, includeStats bool) error {
	if group != "" && !slices.Contains(usage.Groups, group) {
		return &InputError{fmt.Sprintf("unknown quota group %q (known: %s)", group, strings.Join(usage.Groups, ", "))}
	}
	var match func(string) bool
	if group != "" && includeStats {
		match = func(model string) bool { return pool.QuotaGroup(model) == group }
	}
	return c.cfg.Store.ResetKeyUsage(ctx, hash, store.KeyUsageReset{
		Group:        group,
		IncludeStats: includeStats,
		MatchModel:   match,
	})
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
	return c.cfg.Store.KeyTotals(ctx, "")
}

// KeyTotalsFor is one key's successful requests and tokens by model.
func (c *Control) KeyTotalsFor(ctx context.Context, keyHash string) ([]store.KeyTotal, error) {
	return c.cfg.Store.KeyTotals(ctx, keyHash)
}

// KeyQuotas is every key's attributed weekly-quota share by account and
// quota group.
func (c *Control) KeyQuotas(ctx context.Context) ([]store.KeyQuota, error) {
	return c.cfg.Store.KeyQuotas(ctx, "")
}

// KeyQuotaFor is one key's attributed weekly-quota share by account and
// quota group.
func (c *Control) KeyQuotaFor(ctx context.Context, keyHash string) ([]store.KeyQuota, error) {
	return c.cfg.Store.KeyQuotas(ctx, keyHash)
}

// KeySpends is every key's list-price spend in USD by quota group.
func (c *Control) KeySpends(ctx context.Context) (map[string]map[string]float64, error) {
	return c.cfg.Budget.Spends(ctx, "")
}

// KeySpend is one key's list-price spend in USD by quota group.
func (c *Control) KeySpend(ctx context.Context, keyHash string) (map[string]float64, error) {
	return c.cfg.Budget.Spent(ctx, keyHash)
}

// Quota is the weekly-quota use attributed to matching requests.
func (c *Control) Quota(ctx context.Context, f store.RequestFilter) (usage.QuotaReport, error) {
	return usage.Quota(ctx, c.cfg.Store, f)
}

func validateLimits(u store.KeyUpdate) error {
	if u.MaxWorkers != nil && (*u.MaxWorkers < 1 || *u.MaxWorkers > 1024) {
		return &InputError{"max_workers must be between 1 and 1024"}
	}
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
	if err := c.reloadKeys(ctx); err != nil {
		return err
	}
	zero := 0
	c.cfg.Gateway.TrimKeyIdle(hash, &zero)
	return nil
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
	if err != nil {
		return "", err
	}
	return name, c.reloadKeys(ctx)
}

func (c *Control) reloadKeys(ctx context.Context) error {
	keys, err := c.cfg.Store.Keys(ctx)
	if err != nil {
		return err
	}
	byHash := make(map[string]conv.Caller, len(keys))
	all := make(map[string]store.Key, len(keys))
	for _, k := range keys {
		all[k.Hash] = k
		if k.RevokedAt.IsZero() {
			byHash[k.Hash] = conv.Caller{KeyHash: k.Hash, Name: k.Name, Models: k.Models, MaxWorkers: k.MaxWorkers, LimitTotal: k.LimitTotal, Limits: k.Limits}
		}
	}
	c.mu.Lock()
	c.keys = byHash
	c.allKeys = all
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

// ---- Legacy compatibility: separate Argon pool removal --------------------
// Delete dropLegacyArgonSettings and its call in decodeSettings() once
// pre-upgrade settings blobs no longer need migration.

func dropLegacyArgonSettings(s *Settings) {
	delete(s.WarmupModels, "argon")
}

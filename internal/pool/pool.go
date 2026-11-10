// Package pool owns the Antigravity accounts: their directories, health,
// cooldowns, turn slots, and agy processes.
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"agyrelay/internal/agy"
)

// AccountConfig is one configured account.
type AccountConfig struct {
	Name string
	// Home is the account's HOME directory; agy keeps its sign-in below it.
	Home string
	// Env is extra environment for the account's agy (e.g. HTTPS_PROXY).
	Env []string
}

type Config struct {
	Bin      string
	Accounts []AccountConfig
	// TurnsPerAccount caps concurrent turns (running or parked) per account.
	TurnsPerAccount   int
	ProcessPerSession bool
	// CloudCodeEndpoint, when set, routes agy's Cloud Code traffic through
	// a loopback proxy.
	CloudCodeEndpoint string
	Log               *slog.Logger
}

// ErrNoAccount means no account can take the request now.
var ErrNoAccount = errors.New("no Antigravity account available")

type Pool struct {
	cfg      Config
	log      *slog.Logger
	accounts []*Account
}

// Account is one Antigravity sign-in.
type Account struct {
	pool      *Pool
	name      string
	home      string
	geminiDir string
	workspace string
	env       []string

	mu       sync.Mutex
	turns    int
	disabled error
	cooldown map[string]time.Time
	procs    map[string]*procSlot
}

// procSlot single-flights the start of one (account, profile) process.
type procSlot struct {
	proc     *agy.Proc
	starting chan struct{}
	err      error
}

var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func New(cfg Config) (*Pool, error) {
	if len(cfg.Accounts) == 0 {
		return nil, errors.New("no accounts configured")
	}
	p := &Pool{cfg: cfg, log: cfg.Log}
	seen := map[string]bool{}
	for _, ac := range cfg.Accounts {
		if !accountName.MatchString(ac.Name) || seen[ac.Name] {
			return nil, fmt.Errorf("invalid or duplicate account name %q", ac.Name)
		}
		seen[ac.Name] = true
		home, err := filepath.Abs(ac.Home)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", ac.Name, err)
		}
		a := &Account{
			pool:      p,
			name:      ac.Name,
			home:      home,
			geminiDir: filepath.Join(home, ".gemini"),
			workspace: filepath.Join(home, "workspace"),
			env:       ac.Env,
			cooldown:  map[string]time.Time{},
			procs:     map[string]*procSlot{},
		}
		if err := os.MkdirAll(a.workspace, 0o700); err != nil {
			return nil, fmt.Errorf("account %s: %w", ac.Name, err)
		}
		p.accounts = append(p.accounts, a)
	}
	return p, nil
}

func (p *Pool) Accounts() []*Account { return p.accounts }

// Account finds an account by name.
func (p *Pool) Account(name string) *Account {
	for _, a := range p.accounts {
		if a.name == name {
			return a
		}
	}
	return nil
}

// Pick reserves a turn slot on the best account for user and the model's
// quota group, skipping excluded accounts. Accounts are ranked by
// rendezvous hash of (user, account), so a user stays on one account while
// it is healthy and moves to the same fallback when it is not. The caller
// must Release the slot.
func (p *Pool) Pick(user, group string, exclude map[string]bool) (*Account, error) {
	ranked := slices.Clone(p.accounts)
	slices.SortFunc(ranked, func(a, b *Account) int {
		ra, rb := rendezvous(user, a.name), rendezvous(user, b.name)
		switch {
		case ra > rb:
			return -1
		case ra < rb:
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	var reasons []string
	for _, a := range ranked {
		if exclude[a.name] {
			continue
		}
		if err := a.TryAcquire(group); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", a.name, err))
			continue
		}
		return a, nil
	}
	if len(reasons) == 0 {
		return nil, ErrNoAccount
	}
	return nil, fmt.Errorf("%w (%s)", ErrNoAccount, strings.Join(reasons, "; "))
}

func rendezvous(user, account string) uint64 {
	sum := sha256.Sum256([]byte(user + "\x00" + account))
	return binary.BigEndian.Uint64(sum[:8])
}

func (a *Account) Name() string { return a.name }

// GeminiDir is agy's data root for this account.
func (a *Account) GeminiDir() string { return a.geminiDir }

// Workspace is the empty working directory sessions run in.
func (a *Account) Workspace() string { return a.workspace }

// ConversationStore is agy's per-session conversation database.
func (a *Account) ConversationStore(sessionID string) string {
	return filepath.Join(a.geminiDir, "antigravity-cli", "conversations", sessionID+".db")
}

// TryAcquire reserves a turn slot when the account is usable for group.
func (a *Account) TryAcquire(group string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.unavailableLocked(group); err != nil {
		return err
	}
	if a.turns >= a.pool.cfg.TurnsPerAccount {
		return errors.New("all turn slots busy")
	}
	a.turns++
	return nil
}

func (a *Account) Release() {
	a.mu.Lock()
	if a.turns > 0 {
		a.turns--
	}
	a.mu.Unlock()
}

// Available reports why the account cannot serve group now (nil = usable).
func (a *Account) Available(group string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unavailableLocked(group)
}

func (a *Account) unavailableLocked(group string) error {
	if a.disabled != nil {
		return a.disabled
	}
	if until, ok := a.cooldown[group]; ok {
		if time.Now().Before(until) {
			return fmt.Errorf("%s quota cooling down until %s", group, until.Format(time.RFC3339))
		}
		delete(a.cooldown, group)
	}
	return nil
}

// Cool marks group exhausted on this account until the given time.
func (a *Account) Cool(group string, until time.Time) {
	a.mu.Lock()
	a.cooldown[group] = until
	a.mu.Unlock()
	a.pool.log.Warn("account quota cooling down", "account", a.name, "group", group, "until", until)
}

// Disable takes the account out of rotation (e.g. its sign-in expired).
func (a *Account) Disable(reason error) {
	a.mu.Lock()
	a.disabled = reason
	a.mu.Unlock()
	a.pool.log.Error("account disabled", "account", a.name, "reason", reason)
}

// Proc returns the live process for profile, starting it when needed.
// Concurrent callers share one start.
func (a *Account) Proc(ctx context.Context, profile agy.Profile) (*agy.Proc, error) {
	for {
		a.mu.Lock()
		slot := a.procs[profile.Name]
		if slot != nil && slot.starting != nil {
			starting := slot.starting
			a.mu.Unlock()
			select {
			case <-starting:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			a.mu.Lock()
			err := slot.err
			a.mu.Unlock()
			if err != nil {
				return nil, err
			}
			continue
		}
		if slot != nil && slot.proc != nil && slot.proc.Alive() {
			a.mu.Unlock()
			return slot.proc, nil
		}
		slot = &procSlot{starting: make(chan struct{})}
		a.procs[profile.Name] = slot
		a.mu.Unlock()

		proc, err := a.startProc(ctx, profile)
		a.mu.Lock()
		slot.proc, slot.err = proc, err
		close(slot.starting)
		slot.starting = nil
		if err != nil {
			delete(a.procs, profile.Name)
		}
		a.mu.Unlock()
		return proc, err
	}
}

func (a *Account) startProc(ctx context.Context, profile agy.Profile) (*agy.Proc, error) {
	if err := profile.Install(a.geminiDir); err != nil {
		return nil, fmt.Errorf("account %s: %w", a.name, err)
	}
	cfg := a.pool.cfg
	proc, err := agy.StartProc(ctx, agy.ProcConfig{
		Bin:               cfg.Bin,
		Home:              a.home,
		GeminiDir:         a.geminiDir,
		Workdir:           a.workspace,
		Agent:             profile.Name,
		CloudCodeEndpoint: cfg.CloudCodeEndpoint,
		ProcessPerSession: cfg.ProcessPerSession,
		Env:               a.env,
		Log:               a.pool.log.With("account", a.name),
	})
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", a.name, err)
	}
	return proc, nil
}

// Close kills every process of every account.
func (p *Pool) Close() {
	for _, a := range p.accounts {
		a.mu.Lock()
		for _, slot := range a.procs {
			if slot.proc != nil {
				slot.proc.Kill()
			}
		}
		a.mu.Unlock()
	}
}

// QuotaGroup maps a model to the quota bucket agy charges it to. agy groups
// Gemini models, Claude and GPT models, and some dedicated models
// separately; the prefixes follow realagy's observations.
func QuotaGroup(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-oss-"):
		return "claude-gpt"
	case strings.HasPrefix(model, "gemini-4-argon"):
		return "argon"
	default:
		return "gemini"
	}
}

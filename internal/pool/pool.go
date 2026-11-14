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
	"sync/atomic"
	"time"

	"agyrelay/internal/agy"
)

// AccountConfig is one configured account.
type AccountConfig struct {
	Name string
	// Home is the account's HOME directory; agy keeps its sign-in below it.
	Home string
	// Env is extra environment for the account's agy.
	Env []string
	// Proxy is the HTTP proxy URL agy's traffic goes to (the relay's
	// egress proxy); "" leaves agy's environment without one.
	Proxy string
	// CloudCodeEndpoint, when set, is the account's loopback Cloud Code
	// proxy.
	CloudCodeEndpoint string
}

type Config struct {
	Bin      string
	Accounts []AccountConfig
	// TurnsPerAccount caps concurrent turns (running or parked) per account.
	TurnsPerAccount int
	Log             *slog.Logger
}

// ErrNoAccount means no account can take the request now.
var ErrNoAccount = errors.New("no Antigravity account available")

type Pool struct {
	cfg             Config
	log             *slog.Logger
	turnsPerAccount atomic.Int64

	mu       sync.RWMutex
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

	proxy             string
	cloudCodeEndpoint string

	mu    sync.Mutex
	turns int
	// paused is set by the admin; disabled by a failed sign-in; blocked
	// by the control plane (no route for the account's traffic).
	paused   bool
	disabled error
	blocked  error
	// verify is set while Google wants the account's owner to verify the
	// account; verifyURL is Google's page for that ("" when it named none).
	verify    bool
	verifyURL string
	// removed accounts start no processes.
	removed  bool
	cooldown map[string]time.Time
	// modelFailures holds, by session, the failure of its latest
	// generation call; sessions whose latest call succeeded are absent.
	modelFailures map[string]ModelFailure
	procs         map[string]*procSlot
	// models is the ACP model catalog agy last reported for the account,
	// nil until it opens a session.
	models []agy.ModelInfo
	// priority orders accounts in rotation; higher values take requests
	// first.
	priority int
	// allowedModels limits the account to these public model ids; empty
	// allows all models the account offers.
	allowedModels []string
}

// Status is a point-in-time view of an account.
type Status struct {
	Name     string
	Paused   bool
	Disabled string
	Blocked  string
	// Verify is set while Google wants the account verified; VerifyURL is
	// its page for that, "" when it named none.
	Verify    bool
	VerifyURL string
	// Cooldowns maps quota groups to the end of their cooldown.
	Cooldowns map[string]time.Time
	Turns     int
	// Procs counts live agy processes.
	Procs int
}

// procSlot single-flights the start of one (account, profile) process.
type procSlot struct {
	proc     *agy.Proc
	starting chan struct{}
	err      error
}

var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func New(cfg Config) (*Pool, error) {
	p := &Pool{cfg: cfg, log: cfg.Log}
	p.turnsPerAccount.Store(int64(cfg.TurnsPerAccount))
	for _, ac := range cfg.Accounts {
		if _, err := p.Add(ac); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// ValidAccountName reports whether name can name an account (and its
// directory).
func ValidAccountName(name string) bool { return accountName.MatchString(name) && len(name) <= 64 }

// Add registers an account and creates its directories.
func (p *Pool) Add(ac AccountConfig) (*Account, error) {
	if !ValidAccountName(ac.Name) {
		return nil, fmt.Errorf("invalid account name %q", ac.Name)
	}
	home, err := filepath.Abs(ac.Home)
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", ac.Name, err)
	}
	a := &Account{
		pool:              p,
		name:              ac.Name,
		home:              home,
		geminiDir:         filepath.Join(home, ".gemini"),
		workspace:         filepath.Join(home, "workspace"),
		env:               ac.Env,
		proxy:             ac.Proxy,
		cloudCodeEndpoint: ac.CloudCodeEndpoint,
		cooldown:          map[string]time.Time{},
		modelFailures:     map[string]ModelFailure{},
		procs:             map[string]*procSlot{},
	}
	if err := os.MkdirAll(a.workspace, 0o700); err != nil {
		return nil, fmt.Errorf("account %s: %w", ac.Name, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accountLocked(ac.Name) != nil {
		return nil, fmt.Errorf("duplicate account name %q", ac.Name)
	}
	p.accounts = append(p.accounts, a)
	return a, nil
}

// Remove unregisters an account and kills its processes; turns running
// there fail. Its directory stays.
func (p *Pool) Remove(name string) error {
	p.mu.Lock()
	a := p.accountLocked(name)
	if a == nil {
		p.mu.Unlock()
		return fmt.Errorf("no account %q", name)
	}
	p.accounts = slices.DeleteFunc(p.accounts, func(x *Account) bool { return x == a })
	p.mu.Unlock()
	a.mu.Lock()
	a.removed = true
	a.mu.Unlock()
	// The caller deletes the account's files next; agy must be gone.
	for _, proc := range a.killProcs() {
		select {
		case <-proc.Done():
		case <-time.After(removeWait):
			return fmt.Errorf("agy (pid %d) of account %q did not exit", proc.PID(), name)
		}
	}
	return nil
}

// removeWait bounds how long Remove waits for agy to exit after its kill,
// which escalates to SIGKILL within a second.
const removeWait = 5 * time.Second

// SetTurnsPerAccount changes the per-account turn cap; turns running
// above a lowered cap finish normally.
func (p *Pool) SetTurnsPerAccount(n int) { p.turnsPerAccount.Store(int64(n)) }

func (p *Pool) Accounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.accounts)
}

// Account finds an account by name.
func (p *Pool) Account(name string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accountLocked(name)
}

func (p *Pool) accountLocked(name string) *Account {
	for _, a := range p.accounts {
		if a.name == name {
			return a
		}
	}
	return nil
}

// Pick reserves a turn slot on the best account for user and the model's
// quota group, skipping excluded accounts. Accounts are ranked by priority
// (higher first) and then by rendezvous hash of (user, account), so a user
// stays on one account among equals while it is healthy and moves to the
// same fallback when it is not. The caller must Release the slot.
func (p *Pool) Pick(user, group string, exclude map[string]bool) (*Account, error) {
	ranked := p.Accounts()
	slices.SortFunc(ranked, func(a, b *Account) int {
		pa, pb := a.Priority(), b.Priority()
		switch {
		case pa > pb:
			return -1
		case pa < pb:
			return 1
		}
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
	if int64(a.turns) >= a.pool.turnsPerAccount.Load() {
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
	if a.paused {
		return errors.New("paused by the admin")
	}
	if a.disabled != nil {
		return a.disabled
	}
	if a.blocked != nil {
		return a.blocked
	}
	if a.verify {
		return ErrVerificationRequired
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
	wasCooling := time.Now().Before(a.cooldown[group])
	if until.After(a.cooldown[group]) {
		a.cooldown[group] = until
	}
	a.mu.Unlock()
	if !wasCooling {
		a.pool.log.Warn("account quota cooling down", "account", a.name, "group", group, "until", until)
	}
}

// ModelFailure is a generation call agy made for a session that failed.
type ModelFailure struct {
	// Code is the HTTP status agy got.
	Code int
	// Message is the error as agy words it, e.g. "RESOURCE_EXHAUSTED
	// (code 429): Individual quota reached. … Resets in 139h30m19s."
	Message string
	// Reset is when Google said to retry; zero when it named no time.
	Reset time.Time
}

// RecordModelCall notes how the session's latest generation call went;
// failure is nil when it succeeded.
func (a *Account) RecordModelCall(session string, failure *ModelFailure) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if failure == nil {
		delete(a.modelFailures, session)
		return
	}
	a.modelFailures[session] = *failure
}

// ModelFailure returns the failure of the session's latest generation
// call, nil when that call succeeded or none was recorded.
func (a *Account) ModelFailure(session string) *ModelFailure {
	a.mu.Lock()
	defer a.mu.Unlock()
	failure, ok := a.modelFailures[session]
	if !ok {
		return nil
	}
	return &failure
}

// ForgetModelFailure drops the session's recorded failure.
func (a *Account) ForgetModelFailure(session string) {
	a.mu.Lock()
	delete(a.modelFailures, session)
	a.mu.Unlock()
}

// Disable takes the account out of rotation (e.g. its sign-in expired).
func (a *Account) Disable(reason error) {
	a.mu.Lock()
	a.disabled = reason
	a.mu.Unlock()
	a.pool.log.Error("account disabled", "account", a.name, "reason", reason)
}

// SignedIn returns a disabled account to rotation after a new sign-in.
// Its processes are killed so the next turn starts with the new token.
func (a *Account) SignedIn() {
	a.mu.Lock()
	a.disabled = nil
	a.mu.Unlock()
	a.killProcs()
	a.pool.log.Info("account signed in", "account", a.name)
}

func (a *Account) SetPaused(paused bool) {
	a.mu.Lock()
	a.paused = paused
	a.mu.Unlock()
}

// Home is the account's HOME directory.
func (a *Account) Home() string { return a.home }

// Env is the account's extra agy environment. The proxy variables come
// last, so they win over same-named extras. agy 1.5.3 sends all its
// traffic, sign-in included, through them; loopback stays direct.
func (a *Account) Env() []string {
	if a.proxy == "" {
		return a.env
	}
	return append(slices.Clip(a.env), "HTTPS_PROXY="+a.proxy, "HTTP_PROXY="+a.proxy)
}

// SetBlocked takes the account out of rotation while reason is set (e.g.
// its traffic has no route); nil returns it.
func (a *Account) SetBlocked(reason error) {
	a.mu.Lock()
	a.blocked = reason
	a.mu.Unlock()
}

// ErrVerificationRequired keeps an account out of rotation while Google
// wants its owner to verify it.
var ErrVerificationRequired = errors.New("Google asks for the account to be verified by its owner")

// SetVerification records whether Google wants the account's owner to
// verify the account before it serves; url is Google's page for that.
func (a *Account) SetVerification(required bool, url string) {
	a.mu.Lock()
	changed := a.verify != required
	a.verify, a.verifyURL = required, url
	a.mu.Unlock()
	switch {
	case changed && required:
		a.pool.log.Warn("account needs verifying by its owner; out of rotation", "account", a.name)
	case changed:
		a.pool.log.Info("Google no longer asks for the account to be verified", "account", a.name)
	}
}

// Blocked is the reason set by SetBlocked, or nil.
func (a *Account) Blocked() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.blocked
}

func (a *Account) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{Name: a.name, Paused: a.paused, Turns: a.turns, Cooldowns: map[string]time.Time{}}
	if a.disabled != nil {
		st.Disabled = a.disabled.Error()
	}
	if a.blocked != nil {
		st.Blocked = a.blocked.Error()
	}
	st.Verify, st.VerifyURL = a.verify, a.verifyURL
	now := time.Now()
	for group, until := range a.cooldown {
		if now.Before(until) {
			st.Cooldowns[group] = until
		}
	}
	for _, slot := range a.procs {
		if slot.proc != nil && slot.proc.Alive() {
			st.Procs++
		}
	}
	return st
}

// SetModels records the model catalog agy reported for a new session.
func (a *Account) SetModels(models []agy.ModelInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.models = slices.Clone(models)
}

// Models returns the model catalog agy last reported, nil before the
// account's first session.
func (a *Account) Models() []agy.ModelInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.models)
}

// SetRouting sets the account's routing priority and the public model ids
// it may serve (empty allows all models it offers).
func (a *Account) SetRouting(priority int, models []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.priority = priority
	a.allowedModels = slices.Clone(models)
}

func (a *Account) Priority() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.priority
}

// AllowedModels returns the public model ids the account is limited to;
// empty allows all models it offers.
func (a *Account) AllowedModels() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.allowedModels)
}

// AllowsModel reports whether the account may serve the public model id.
func (a *Account) AllowsModel(modelID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.allowedModels) == 0 || slices.Contains(a.allowedModels, modelID)
}

// Restart kills the account's agy processes; turns running there fail
// and the next request starts agy again.
func (a *Account) Restart() {
	a.killProcs()
	a.pool.log.Info("account restarted", "account", a.name)
}

// LiveProcs returns the account's running agy processes.
func (a *Account) LiveProcs() []*agy.Proc {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*agy.Proc
	for _, slot := range a.procs {
		if slot.proc != nil && slot.proc.Alive() {
			out = append(out, slot.proc)
		}
	}
	return out
}

// killProcs kills the running processes and forgets them at once, so the
// next Proc starts a new one instead of returning a process still dying.
// It returns the killed processes.
func (a *Account) killProcs() []*agy.Proc {
	a.mu.Lock()
	defer a.mu.Unlock()
	var killed []*agy.Proc
	for name, slot := range a.procs {
		if slot.proc != nil {
			slot.proc.Kill()
			killed = append(killed, slot.proc)
			delete(a.procs, name)
		}
	}
	return killed
}

// Proc returns the live process for profile, starting it when needed.
// Concurrent callers share one start.
func (a *Account) Proc(ctx context.Context, profile agy.Profile) (*agy.Proc, error) {
	for {
		a.mu.Lock()
		if a.removed {
			a.mu.Unlock()
			return nil, fmt.Errorf("account %s was removed", a.name)
		}
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
		if err == nil && a.removed {
			proc.Kill()
			proc, err = nil, fmt.Errorf("account %s was removed", a.name)
		}
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
		CloudCodeEndpoint: a.cloudCodeEndpoint,
		Env:               a.Env(),
		Log:               a.pool.log.With("account", a.name),
	})
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", a.name, err)
	}
	return proc, nil
}

// Close kills every process of every account.
func (p *Pool) Close() {
	for _, a := range p.Accounts() {
		a.killProcs()
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

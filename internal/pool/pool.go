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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"agyrelay/internal/agy"
)

// AccountConfig is one configured account.
type AccountConfig struct {
	// ID is the immutable identifier (used for directory, egress, and routing
	// keys). When empty in tests, Name is used.
	ID   string
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
	id        string
	home      string
	geminiDir string
	workspace string
	env       []string

	proxy             string
	cloudCodeEndpoint string

	mu    sync.Mutex
	name  string
	turns int
	// runs maps the turns that make model calls to when they began; a turn
	// waiting for tool results is not in it.
	runs map[any]time.Time
	// paused is set by the admin or automatically (with pauseReason);
	// disabled by a failed sign-in; blocked by the control plane (no route
	// for the account's traffic).
	paused      bool
	pauseReason string
	disabled    error
	blocked     error
	// verify is set while Google wants the account's owner to verify the
	// account; verifyURL is Google's page for that ("" when it named none).
	verify    bool
	verifyURL string
	// removed accounts start no processes.
	removed  bool
	cooldown map[string]Cooldown
	// modelFailures holds, by session, the failure of its latest
	// generation call; sessions whose latest call succeeded are absent.
	modelFailures map[string]ModelFailure
	// calls is the generation calls of the last callHistory, oldest
	// first; streaks counts, by session, its consecutive calls refused
	// with a 429 that names no wait.
	calls   []callRecord
	streaks map[string]*noWaitStreak
	// failedCalls counts, by session, its consecutive failed calls.
	failedCalls map[string]int
	// truncated holds the sessions whose latest generation call stopped
	// at the output token limit.
	truncated map[string]bool
	procs     map[string]*procSlot
	// models is the ACP model catalog agy last reported for the account,
	// nil until it opens a session.
	models []agy.ModelInfo
	// quotaGroups holds the quota groups with a bucket in the account's
	// latest quota sample, nil until it was sampled.
	quotaGroups map[string]bool
	// priority orders accounts in rotation; higher values take requests
	// first.
	priority int
	// allowedModels limits the account to these public model ids; empty
	// allows all models the account offers.
	allowedModels []string
}

// Cooldown is an active quota-group cooldown on an account.
type Cooldown struct {
	Until time.Time
	SetAt time.Time
}

// Status is a point-in-time view of an account.
type Status struct {
	ID          string
	Name        string
	Paused      bool
	PauseReason string
	Disabled    string
	Blocked     string
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

var accountID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

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

// ValidAccountID reports whether id is safe as an account directory and proxy
// identifier.
func ValidAccountID(id string) bool { return accountID.MatchString(id) && len(id) <= 64 }

// ValidAccountName reports whether name can be an account's display name.
func ValidAccountName(name string) bool {
	return name != "" && len(name) <= 64 && !strings.ContainsFunc(name, unicode.IsControl)
}

// Add registers an account and creates its directories.
func (p *Pool) Add(ac AccountConfig) (*Account, error) {
	id := ac.ID
	if id == "" {
		id = ac.Name
	}
	if !ValidAccountID(id) {
		return nil, fmt.Errorf("invalid account id %q", id)
	}
	if !ValidAccountName(ac.Name) {
		return nil, fmt.Errorf("invalid account name %q", ac.Name)
	}
	home, err := filepath.Abs(ac.Home)
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", ac.Name, err)
	}
	a := &Account{
		pool:              p,
		id:                id,
		name:              ac.Name,
		home:              home,
		geminiDir:         filepath.Join(home, ".gemini"),
		workspace:         filepath.Join(home, "workspace"),
		env:               ac.Env,
		proxy:             ac.Proxy,
		cloudCodeEndpoint: ac.CloudCodeEndpoint,
		runs:              map[any]time.Time{},
		cooldown:          map[string]Cooldown{},
		modelFailures:     map[string]ModelFailure{},
		streaks:           map[string]*noWaitStreak{},
		failedCalls:       map[string]int{},
		truncated:         map[string]bool{},
		procs:             map[string]*procSlot{},
	}
	if err := os.MkdirAll(a.workspace, 0o700); err != nil {
		return nil, fmt.Errorf("account %s: %w", ac.Name, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accountLocked(id) != nil {
		return nil, fmt.Errorf("duplicate account id %q", id)
	}
	p.accounts = append(p.accounts, a)
	return a, nil
}

// Remove unregisters an account and kills its processes; turns running
// there fail. Its directory stays.
func (p *Pool) Remove(id string) error {
	p.mu.Lock()
	a := p.accountLocked(id)
	if a == nil {
		p.mu.Unlock()
		return fmt.Errorf("no account %q", id)
	}
	p.accounts = slices.DeleteFunc(p.accounts, func(x *Account) bool { return x == a })
	p.mu.Unlock()
	a.mu.Lock()
	a.removed = true
	name := a.name
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

// SetTurnsPerAccount changes the per-account worker cap; turns running
// above a lowered cap finish normally.
func (p *Pool) SetTurnsPerAccount(n int) { p.turnsPerAccount.Store(int64(n)) }

// TurnsPerAccount returns the per-account worker cap.
func (p *Pool) TurnsPerAccount() int { return int(p.turnsPerAccount.Load()) }

func (p *Pool) Accounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.accounts)
}

// Account finds an account by ID.
func (p *Pool) Account(id string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accountLocked(id)
}

func (p *Pool) accountLocked(id string) *Account {
	for _, a := range p.accounts {
		if a.id == id {
			return a
		}
	}
	return nil
}

// IdleSessions describes an account's idle workers in agy.
type IdleSessions struct {
	Count  int
	Oldest time.Time
}

// Pick reserves a turn slot on the best account for session and model, by
// the quota group each account charges the model to, skipping excluded
// accounts (by ID). It first tries accounts with a
// free worker slot (busy + idle < TurnsPerAccount), ranked by priority and
// rendezvous hash of (session, account ID), and only falls back to accounts that
// must evict an idle worker (oldest idle session first within the same
// priority) when every eligible account is full. The caller must Release
// the slot.
func (p *Pool) Pick(session, model string, exclude map[string]bool, idle map[string]IdleSessions) (*Account, error) {
	ranked := p.Accounts()
	slices.SortFunc(ranked, func(a, b *Account) int {
		return compareAccounts(session, a, b, nil)
	})
	for _, a := range ranked {
		if exclude[a.id] {
			continue
		}
		if err := a.tryAcquire(a.PoolGroup(model), idle[a.id].Count); err == nil {
			return a, nil
		}
	}
	slices.SortFunc(ranked, func(a, b *Account) int {
		return compareAccounts(session, a, b, idle)
	})
	var reasons []string
	for _, a := range ranked {
		if exclude[a.id] {
			continue
		}
		if err := a.TryAcquire(a.PoolGroup(model)); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", a.Name(), err))
			continue
		}
		return a, nil
	}
	if len(reasons) == 0 {
		return nil, ErrNoAccount
	}
	return nil, fmt.Errorf("%w (%s)", ErrNoAccount, strings.Join(reasons, "; "))
}

func compareAccounts(session string, a, b *Account, idle map[string]IdleSessions) int {
	pa, pb := a.Priority(), b.Priority()
	switch {
	case pa > pb:
		return -1
	case pa < pb:
		return 1
	}
	if idle != nil {
		ia, ib := idle[a.id], idle[b.id]
		if (ia.Count > 0) != (ib.Count > 0) {
			if ia.Count > 0 {
				return -1
			}
			return 1
		}
		if ia.Count > 0 && !ia.Oldest.Equal(ib.Oldest) {
			if ia.Oldest.Before(ib.Oldest) {
				return -1
			}
			return 1
		}
	}
	ra, rb := rendezvous(session, a.id), rendezvous(session, b.id)
	switch {
	case ra > rb:
		return -1
	case ra < rb:
		return 1
	}
	return strings.Compare(a.id, b.id)
}

func rendezvous(session, account string) uint64 {
	sum := sha256.Sum256([]byte(session + "\x00" + account))
	return binary.BigEndian.Uint64(sum[:8])
}

func (a *Account) ID() string { return a.id }

func (a *Account) Name() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.name
}

func (a *Account) SetName(name string) {
	a.mu.Lock()
	a.name = name
	a.mu.Unlock()
}

// GeminiDir is agy's data root for this account.
func (a *Account) GeminiDir() string { return a.geminiDir }

// Workspace is the empty working directory sessions run in.
func (a *Account) Workspace() string { return a.workspace }

// ConversationStore is agy's per-session conversation database.
func (a *Account) ConversationStore(sessionID string) string {
	return filepath.Join(a.geminiDir, "antigravity-cli", "conversations", sessionID+".db")
}

// ErrSlotBusy means all worker slots on the account are busy with active turns.
var ErrSlotBusy = errors.New("all worker slots busy")

// TryAcquire reserves a turn slot when the account is usable for group.
func (a *Account) TryAcquire(group string) error {
	return a.tryAcquire(group, 0)
}

func (a *Account) tryAcquire(group string, idle int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.unavailableLocked(group); err != nil {
		return err
	}
	if int64(a.turns+idle) >= a.pool.turnsPerAccount.Load() {
		return ErrSlotBusy
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

// Turns returns the number of active (running or parked) turns on the account.
func (a *Account) Turns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns
}

// BeginRun records that the turn identified by key began making model
// calls at since: it started, or resumed after tool results.
func (a *Account) BeginRun(key any, since time.Time) {
	a.mu.Lock()
	a.runs[key] = since
	a.mu.Unlock()
}

// EndRun records that the turn identified by key makes no model call: it
// waits for tool results or ended. It is a no-op for a turn not running.
func (a *Account) EndRun(key any) {
	a.mu.Lock()
	delete(a.runs, key)
	a.mu.Unlock()
}

// OldestRun returns when the longest running turn began; ok is false when
// no turn is making model calls.
func (a *Account) OldestRun() (since time.Time, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, began := range a.runs {
		if !ok || began.Before(since) {
			since, ok = began, true
		}
	}
	return since, ok
}

// InRotation reports whether the account is eligible to take traffic
// (not paused, disabled, blocked, or awaiting owner verification).
func (a *Account) InRotation() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inRotationLocked()
}

func (a *Account) inRotationLocked() bool {
	return !a.paused && a.disabled == nil && a.blocked == nil && !a.verify
}

// Available reports why the account cannot serve group now (nil = usable).
func (a *Account) Available(group string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unavailableLocked(group)
}

// CooldownRemaining returns how much longer group is cooling down when
// cooldown is the only reason the account cannot serve group (0 otherwise).
func (a *Account) CooldownRemaining(group string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.inRotationLocked() {
		return 0
	}
	entry, ok := a.cooldown[group]
	if !ok {
		return 0
	}
	rem := time.Until(entry.Until)
	if rem <= 0 {
		delete(a.cooldown, group)
		return 0
	}
	return rem
}

// Cooldowns returns the account's active quota-group cooldowns with when each
// was recorded.
func (a *Account) Cooldowns() map[string]Cooldown {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	out := make(map[string]Cooldown, len(a.cooldown))
	for group, entry := range a.cooldown {
		if now.Before(entry.Until) {
			out[group] = entry
		}
	}
	return out
}

func (a *Account) unavailableLocked(group string) error {
	if a.paused {
		if a.pauseReason != "" {
			return errors.New("paused: " + a.pauseReason)
		}
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
	if entry, ok := a.cooldown[group]; ok {
		if time.Now().Before(entry.Until) {
			return fmt.Errorf("%s quota cooling down until %s", group, entry.Until.Format(time.RFC3339))
		}
		delete(a.cooldown, group)
	}
	return nil
}

// Cool marks group exhausted on this account until the given time.
func (a *Account) Cool(group string, until time.Time) {
	a.mu.Lock()
	now := time.Now()
	cur := a.cooldown[group]
	wasCooling := now.Before(cur.Until)
	if until.After(cur.Until) {
		a.cooldown[group] = Cooldown{Until: until, SetAt: now}
	}
	name := a.name
	a.mu.Unlock()
	if !wasCooling && now.Before(until) {
		a.pool.log.Warn("account quota cooling down", "account", name, "group", group, "until", until)
	}
}

// SetCooldown replaces group's cooldown with until (or clears it when until is
// zero or not in the future), unless a newer cooldown was recorded after asOf.
func (a *Account) SetCooldown(group string, until, asOf time.Time) {
	a.mu.Lock()
	cooled, cleared := a.setCooldownLocked(group, until, asOf, time.Now())
	name := a.name
	a.mu.Unlock()

	switch {
	case cooled:
		a.pool.log.Warn("account quota cooling down", "account", name, "group", group, "until", until)
	case cleared:
		a.pool.log.Info("account quota cooldown cleared", "account", name, "group", group)
	}
}

// SyncCooldowns aligns every quota group's cooldown with a fresh quota sample
// started at asOf: exhausted groups cool until their reset time, and groups
// with quota available (or absent from the sample) clear older cooldowns.
// Cooldowns recorded after asOf are preserved unless the sample extends them.
func (a *Account) SyncCooldowns(untilByGroup map[string]time.Time, asOf time.Time) {
	a.mu.Lock()
	now := time.Now()
	var cooled []string
	var cleared []string

	for group := range a.cooldown {
		if _, inSample := untilByGroup[group]; inSample {
			continue
		}
		if _, didClear := a.setCooldownLocked(group, time.Time{}, asOf, now); didClear {
			cleared = append(cleared, group)
		}
	}

	for group, until := range untilByGroup {
		didCool, didClear := a.setCooldownLocked(group, until, asOf, now)
		if didCool {
			cooled = append(cooled, group)
		}
		if didClear {
			cleared = append(cleared, group)
		}
	}

	name := a.name
	a.mu.Unlock()

	for _, group := range cooled {
		a.pool.log.Warn("account quota cooling down", "account", name, "group", group, "until", untilByGroup[group])
	}
	for _, group := range cleared {
		a.pool.log.Info("account quota cooldown cleared", "account", name, "group", group)
	}
}

func (a *Account) setCooldownLocked(group string, until, asOf, now time.Time) (cooled, cleared bool) {
	cur := a.cooldown[group]
	wasCooling := now.Before(cur.Until)
	cooling := now.Before(until)

	newerRecorded := !asOf.IsZero() && cur.SetAt.After(asOf)
	if newerRecorded {
		if until.After(cur.Until) {
			a.cooldown[group] = Cooldown{Until: until, SetAt: now}
			return !wasCooling && cooling, false
		}
		return false, false
	}

	if cooling {
		setAt := now
		if wasCooling && until.Equal(cur.Until) {
			setAt = cur.SetAt
		}
		a.cooldown[group] = Cooldown{Until: until, SetAt: setAt}
		return !wasCooling, false
	}

	delete(a.cooldown, group)
	return false, wasCooling
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
	// Truncated marks a call that answered but stopped at the output
	// token limit (Code is 200): the reply is cut off, not failed.
	Truncated bool
}

// Risk control: Google answers a request it dislikes with a 429 that names
// no wait, while the account's other requests go through. A real quota
// or capacity 429 names a wait, so the same session getting such a 429
// again and again is already suspect.
const (
	// callHistory is how long the account remembers its generation calls.
	callHistory = 10 * time.Minute
	// riskStreakWithPeers is how many such 429s in a row, while other
	// sessions' calls on the same model succeed, show risk control.
	riskStreakWithPeers = 2
	// riskStreakAlone is how many show it when no other session made a
	// call on the model meanwhile.
	riskStreakAlone = 3
	// stuckCallStreak is how many failed calls in a row, with no call
	// succeeding between, show a session stuck in agy's retry loop: agy
	// retries a failing call 8 times, but a call that answers with
	// nothing usable is retried at once, without end.
	stuckCallStreak = 10
)

// CallVerdict is what a generation call shows about its session.
type CallVerdict int

const (
	CallOK CallVerdict = iota
	// CallRisky: the session's request is under risk control.
	CallRisky
	// CallStuck: the session keeps failing calls, see stuckCallStreak.
	// Reported once per turn.
	CallStuck
)

// callRecord is one generation call's outcome.
type callRecord struct {
	at      time.Time
	session string
	model   string
	ok      bool
	// noWait is a 429 that names no wait.
	noWait bool
}

// noWaitStreak is a session's run of 429s that name no wait.
type noWaitStreak struct {
	since time.Time
	count int
	// risky is set once the streak was judged risk control.
	risky bool
}

func isNoWait429(failure *ModelFailure) bool {
	return failure != nil && failure.Code == http.StatusTooManyRequests && failure.Reset.IsZero()
}

// RecordModelCall notes how the session's latest generation call, made
// on model, went; failure is nil when it succeeded. A Truncated failure
// counts as a call that answered; Truncated tells whether it was the
// latest. It reports CallRisky when the call shows the session's request
// is under risk control, once per
// session turn: the call is the session's second 429 in a row that names
// no wait while other sessions' calls on the model succeeded, or its
// third when no other session made a call on the model meanwhile. When
// another session's call on the model was refused the same way, the
// trouble is the model's or the account's, not the request's. A call is
// also the stuckCallStreak-th failed one in a row: CallStuck, once per
// session turn.
func (a *Account) RecordModelCall(session, model string, failure *ModelFailure) CallVerdict {
	a.mu.Lock()
	now := time.Now()
	a.pruneCallsLocked(now)
	noWait := isNoWait429(failure)
	failed := failure != nil && !failure.Truncated
	a.calls = append(a.calls, callRecord{at: now, session: session, model: model, ok: !failed, noWait: noWait})
	delete(a.modelFailures, session)
	delete(a.truncated, session)
	switch {
	case failed:
		a.modelFailures[session] = *failure
		a.failedCalls[session]++
	case failure != nil:
		a.truncated[session] = true
		delete(a.failedCalls, session)
	default:
		delete(a.failedCalls, session)
	}
	var risk bool
	if noWait {
		risk = a.countNoWaitLocked(session, model, now)
	} else {
		delete(a.streaks, session)
	}
	stuck := failed && a.failedCalls[session] == stuckCallStreak
	name := a.name
	a.mu.Unlock()
	switch {
	case risk:
		a.pool.log.Warn("request under risk control", "account", name, "session", session, "model", model)
		return CallRisky
	case stuck:
		a.pool.log.Warn("session stuck failing generation calls", "account", name, "session", session, "model", model, "calls", stuckCallStreak)
		return CallStuck
	}
	return CallOK
}

func (a *Account) pruneCallsLocked(now time.Time) {
	keep := 0
	for keep < len(a.calls) && now.Sub(a.calls[keep].at) > callHistory {
		keep++
	}
	a.calls = a.calls[keep:]
}

// countNoWaitLocked adds a 429 that names no wait to the session's
// streak and judges the streak.
func (a *Account) countNoWaitLocked(session, model string, now time.Time) bool {
	streak := a.streaks[session]
	if streak == nil {
		streak = &noWaitStreak{since: now}
		a.streaks[session] = streak
	}
	streak.count++
	if streak.risky {
		return false
	}
	peersServed, peersRefused := false, false
	for _, c := range a.calls {
		if c.session == session || c.model != model || c.at.Before(streak.since) {
			continue
		}
		peersServed = peersServed || c.ok
		peersRefused = peersRefused || c.noWait
	}
	if peersRefused {
		return false
	}
	streak.risky = streak.count >= riskStreakAlone || peersServed && streak.count >= riskStreakWithPeers
	return streak.risky
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

// Truncated reports whether the session's latest generation call stopped
// at the output token limit.
func (a *Account) Truncated(session string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.truncated[session]
}

// ForgetModelFailure drops the session's recorded failure, its run of
// failed calls, its truncated call, and its run of 429s that name no wait.
func (a *Account) ForgetModelFailure(session string) {
	a.mu.Lock()
	delete(a.modelFailures, session)
	delete(a.failedCalls, session)
	delete(a.truncated, session)
	delete(a.streaks, session)
	a.mu.Unlock()
}

// Disable takes the account out of rotation (e.g. its sign-in expired).
func (a *Account) Disable(reason error) {
	a.mu.Lock()
	a.disabled = reason
	name := a.name
	a.mu.Unlock()
	a.pool.log.Error("account disabled", "account", name, "reason", reason)
}

// SignedIn returns a disabled account to rotation after a new sign-in.
// Its processes are killed so the next turn starts with the new token.
func (a *Account) SignedIn() {
	a.mu.Lock()
	a.disabled = nil
	name := a.name
	a.mu.Unlock()
	a.killProcs()
	a.pool.log.Info("account signed in", "account", name)
}

// Enable returns a disabled account to rotation when its sign-in works
// again (e.g. after a transient auth error), keeping running processes.
func (a *Account) Enable() {
	a.mu.Lock()
	wasDisabled := a.disabled != nil
	a.disabled = nil
	name := a.name
	a.mu.Unlock()
	if wasDisabled {
		a.pool.log.Info("account re-enabled", "account", name)
	}
}

func (a *Account) SetPaused(paused bool) {
	a.mu.Lock()
	a.paused = paused
	a.pauseReason = ""
	a.mu.Unlock()
}

// PauseWithReason takes the account out of rotation with a persisted reason
// (e.g. Google rejected its exit location) until manually resumed. It reports
// whether the pause state or reason changed.
func (a *Account) PauseWithReason(reason string) bool {
	a.mu.Lock()
	changed := !a.paused || a.pauseReason != reason
	a.paused = true
	a.pauseReason = reason
	a.mu.Unlock()
	return changed
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
	name := a.name
	a.mu.Unlock()
	switch {
	case changed && required:
		a.pool.log.Warn("account needs verifying by its owner; out of rotation", "account", name)
	case changed:
		a.pool.log.Info("Google no longer asks for the account to be verified", "account", name)
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
	st := Status{ID: a.id, Name: a.name, Paused: a.paused, PauseReason: a.pauseReason, Turns: a.turns, Cooldowns: map[string]time.Time{}}
	if a.disabled != nil {
		st.Disabled = a.disabled.Error()
	}
	if a.blocked != nil {
		st.Blocked = a.blocked.Error()
	}
	st.Verify, st.VerifyURL = a.verify, a.verifyURL
	now := time.Now()
	for group, entry := range a.cooldown {
		if now.Before(entry.Until) {
			st.Cooldowns[group] = entry.Until
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
	a.pool.log.Info("account restarted", "account", a.Name())
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

// RetireIdleProcs kills the account's agy processes that hold no session
// and have been idle for idle, and forgets them; the next Proc starts a
// new one. It returns how many it killed.
//
// Proc hands out a process under the same lock, so a caller that just got
// one is not retired before it opens its session.
func (a *Account) RetireIdleProcs(idle time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	retired := 0
	for name, slot := range a.procs {
		if slot.proc == nil || !slot.proc.RetireIfIdle(idle) {
			continue
		}
		delete(a.procs, name)
		retired++
		a.pool.log.Info("idle agy process retired", "account", a.name, "agent", name, "pid", slot.proc.PID())
	}
	return retired
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
			slot.proc.Touch()
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
	name := a.Name()
	if err := profile.Install(a.geminiDir); err != nil {
		return nil, fmt.Errorf("account %s: %w", name, err)
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
		Log:               a.pool.log.With("account", name),
	})
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", name, err)
	}
	return proc, nil
}

// Close kills every process of every account.
func (p *Pool) Close() {
	for _, a := range p.Accounts() {
		a.killProcs()
	}
}

// Quota group ids.
const (
	GroupGemini    = "gemini"
	GroupClaudeGPT = "claude-gpt"
	GroupArgon     = "argon"
)

const (
	argonPrefix  = "gemini-4-argon"
	carbonPrefix = "gemini-4-carbon"
)

// QuotaGroup maps a model to the quota bucket agy charges it to on a Pro
// plan, which is what a request has before an account serves it. agy
// groups Gemini models, Claude and GPT models, and some dedicated models
// separately. Use Account.PoolGroup once the account is known.
func QuotaGroup(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-oss-"):
		return GroupClaudeGPT
	case strings.HasPrefix(model, argonPrefix):
		return GroupArgon
	default:
		return GroupGemini
	}
}

// KeyGroup maps a model to the group a key's spend limit covers. It
// follows the model name alone, whichever quota bucket the serving
// account charges: Argon and Carbon share one group.
func KeyGroup(model string) string {
	if strings.HasPrefix(model, carbonPrefix) {
		return GroupArgon
	}
	return QuotaGroup(model)
}

// PoolGroup maps a model to the quota group this account charges it to.
// An account that offers Gemini 4 Carbon and reports no Argon bucket (an
// Ultra plan) charges Argon and Carbon to the Gemini bucket; otherwise
// both go to the Argon bucket.
func (a *Account) PoolGroup(model string) string {
	if !strings.HasPrefix(model, argonPrefix) && !strings.HasPrefix(model, carbonPrefix) {
		return QuotaGroup(model)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sharesGeminiBucketLocked() {
		return GroupGemini
	}
	return GroupArgon
}

func (a *Account) sharesGeminiBucketLocked() bool {
	sampled := a.quotaGroups != nil
	offersCarbon := slices.ContainsFunc(a.models, func(m agy.ModelInfo) bool {
		return strings.HasPrefix(m.ModelID, carbonPrefix)
	})
	return sampled && !a.quotaGroups[GroupArgon] && offersCarbon
}

// SetQuotaGroups records the quota groups that have a bucket in the
// account's latest quota sample.
func (a *Account) SetQuotaGroups(groups []string) {
	set := make(map[string]bool, len(groups))
	for _, group := range groups {
		set[group] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.quotaGroups = set
}

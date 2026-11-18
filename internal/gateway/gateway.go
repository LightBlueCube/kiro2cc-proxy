// Package gateway turns completion requests into agy turns. ACP sessions
// are a cache of client transcripts: a request continues a turn parked on
// a client tool, reuses a session whose snapshot is a prefix of the
// request (warm), or replays the transcript into a new session (fresh).
package gateway

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
	"agyrelay/internal/sysprompt"
)

const (
	// defaultParkTTL cancels a turn whose client never returns its tool
	// results, when Config.ParkTTL is zero.
	defaultParkTTL        = 5 * time.Minute
	janitorInterval       = 30 * time.Second
	maxRetryableQuotaWait = time.Minute
)

type Config struct {
	Pool   *pool.Pool
	Bridge *bridge.Server
	// DataDir holds snapshots.json.
	DataDir string
	// SnapshotTTL bounds how long an idle session stays reusable.
	SnapshotTTL time.Duration
	// IdleClose is how long a session stays open in agy without a turn;
	// then it is closed (under --process-per-session its worker exits) and
	// the next request of the conversation reopens it from disk.
	IdleClose time.Duration
	// ParkTTL bounds how long a turn waits for the client's tool results
	// before it is cancelled (0 = defaultParkTTL).
	ParkTTL time.Duration
	// MaxWorkers caps open session workers across all accounts (0 =
	// unlimited); excess idle sessions are closed first.
	MaxWorkers int
	// Recorder, when set, receives a record of every request.
	Recorder Recorder
	// Budget measures key spend; it must be set when keys have limits.
	Budget Budget
	// Refresh, when set, samples an account's quota, starting agy, which
	// reports the account's model catalog (see refreshUnreported).
	Refresh func(ctx context.Context, account *pool.Account) error
	// KeepForQuota, when set, is asked before an account's last open
	// session closes, idle since lastUsed: true keeps it open, and agy
	// running, until the quota shows the account's use. Asked again on
	// every janitor pass.
	KeepForQuota func(ctx context.Context, account *pool.Account, lastUsed time.Time) bool
	// RefuseHarnessPrompts refuses requests whose system prompt is still a
	// harness's default, as HarnessSignatures recognize it.
	RefuseHarnessPrompts bool
	HarnessSignatures    []sysprompt.Signature
	// Flags, when set, remembers the system prompts that Google's risk
	// control refused, and the gateway refuses them (see risk.go).
	Flags PromptFlags
	Log   *slog.Logger
}

// Budget reports API keys' list-price spend.
type Budget interface {
	// Spent is the key's spend in USD by quota group.
	Spent(ctx context.Context, keyHash string) (map[string]float64, error)
	// Priced reports whether the model has a price, which spend needs.
	Priced(modelID string) bool
}

type Gateway struct {
	log           *slog.Logger
	pool          *pool.Pool
	bridge        *bridge.Server
	snaps         *snapshotStore
	conversations *conversations
	recorder      Recorder
	budget        Budget
	flags         PromptFlags
	refresh       func(ctx context.Context, account *pool.Account) error
	keepForQuota  func(ctx context.Context, account *pool.Account, lastUsed time.Time) bool
	// refreshes holds, by account name, a running refresh or the last one
	// that left the account without a catalog.
	refreshMu sync.Mutex
	refreshes map[string]*refreshCall
	stop      chan struct{}
	// idleClose and parkTTL are time.Durations.
	idleClose  atomic.Int64
	parkTTL    atomic.Int64
	maxWorkers atomic.Int64
	// harnessPrompts recognizes harnesses' default system prompts; nil while
	// their refusal is off.
	harnessPrompts atomic.Pointer[sysprompt.Detector]

	// mu guards sessions and every session's and activeTurn's mutable state.
	mu        sync.Mutex
	sessions  map[string]*session // sessionRef → session
	busyTotal int
	busyByKey map[string]int
}

// session is an ACP session known to this process.
type session struct {
	ref     string
	id      string
	user    string
	account *pool.Account
	proc    *agy.Proc
	binding *bridge.Binding
	// known lists the client tool names the session was opened with.
	known    map[string]bool
	lastUsed time.Time
	// turn is the running or parked turn, or a claim while one starts.
	turn *activeTurn
}

// activeTurn is one agy prompt. It may span several HTTP responses: each
// client tool call ends a response, and the request carrying the results
// continues the same turn.
type activeTurn struct {
	sess    *session
	account *pool.Account
	// choice is the model and effort as the client asked for them; slug
	// and tier are what the account resolved them to.
	choice     string
	slug, tier string
	// turn is nil while the claim is being prepared.
	turn   *agy.Turn
	groups map[string]*callGroup
	// parked is the group the last response ended on, since parkedAt.
	parked    *callGroup
	parkedAt  time.Time
	parkTimer *time.Timer
	// started is when the prompt was launched.
	started time.Time
	// consuming is true while a response reads the turn's events.
	consuming        bool
	cancelledByAdmin bool
	// risky is set when the turn was cancelled because Google's risk
	// control refused its request; riskyRequest and riskyResponse are the
	// upstream generation call that tripped it.
	risky                       bool
	riskyRequest, riskyResponse []byte
	finished                    bool
	// baseSteps is a lower bound of the session's native steps at launch.
	baseSteps int
	// The turn's earlier responses reported the model calls before step
	// usageSteps; reported is their sum. Only the consuming response uses
	// them.
	usageSteps int
	reported   conv.Usage
	// endSteps is the native step count after the turn ended normally, 0
	// when unknown. It is set before closed is closed.
	endSteps int
	// closed is closed after the settled turn was cleaned up.
	closed chan struct{}
	// closing marks a claim held while the idle session is closed in agy;
	// closed is closed when that is done.
	closing bool
	// callToken is the Record.CallToken of the request the turn serves now.
	callToken string
}

func newActiveTurn(sess *session) *activeTurn {
	return &activeTurn{sess: sess, groups: map[string]*callGroup{}, closed: make(chan struct{})}
}

func New(cfg Config) (*Gateway, error) {
	snaps, err := openSnapshotStore(filepath.Join(cfg.DataDir, "snapshots.json"), cfg.SnapshotTTL)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		log:           cfg.Log,
		pool:          cfg.Pool,
		bridge:        cfg.Bridge,
		snaps:         snaps,
		recorder:      cfg.Recorder,
		budget:        cfg.Budget,
		flags:         cfg.Flags,
		refresh:       cfg.Refresh,
		keepForQuota:  cfg.KeepForQuota,
		refreshes:     map[string]*refreshCall{},
		stop:          make(chan struct{}),
		sessions:      map[string]*session{},
		conversations: newConversations(),
		busyByKey:     map[string]int{},
	}
	parkTTL := cfg.ParkTTL
	if parkTTL <= 0 {
		parkTTL = defaultParkTTL
	}
	g.idleClose.Store(int64(cfg.IdleClose))
	g.parkTTL.Store(int64(parkTTL))
	g.maxWorkers.Store(int64(cfg.MaxWorkers))
	g.SetHarnessPrompts(cfg.HarnessSignatures, cfg.RefuseHarnessPrompts)
	go g.janitor()
	return g, nil
}

// SetIdleClose changes how long an idle session stays open in agy.
func (g *Gateway) SetIdleClose(d time.Duration) { g.idleClose.Store(int64(d)) }

// SetParkTTL changes how long a parked turn waits for tool results and
// reschedules any turns already waiting.
func (g *Gateway) SetParkTTL(d time.Duration) {
	if d <= 0 {
		d = defaultParkTTL
	}
	g.mu.Lock()
	g.parkTTL.Store(int64(d))
	for _, sess := range g.sessions {
		at := sess.turn
		if at == nil || at.parked == nil || at.finished || at.parkTimer == nil {
			continue
		}
		at.parkTimer.Reset(max(0, time.Until(at.parkedAt.Add(d))))
	}
	g.mu.Unlock()
}

// SetHarnessPrompts replaces the signatures of harnesses' default system
// prompts and turns their refusal on or off.
func (g *Gateway) SetHarnessPrompts(signatures []sysprompt.Signature, refuse bool) {
	if !refuse {
		g.harnessPrompts.Store(nil)
		return
	}
	g.harnessPrompts.Store(sysprompt.NewDetector(signatures))
}

// SetMaxWorkers changes the global worker cap (0 = unlimited) and closes
// idle sessions above the new global or per-account caps.
func (g *Gateway) SetMaxWorkers(n int) {
	g.maxWorkers.Store(int64(n))
	g.mu.Lock()
	evicted := g.evictIdleLocked("", nil, nil)
	g.mu.Unlock()
	g.closeEvicted(evicted)
}

// TrimKeyIdle closes idle sessions of keyHash above maxWorkers (0 closes all
// of the key's idle sessions; nil is unlimited).
func (g *Gateway) TrimKeyIdle(keyHash string, maxWorkers *int) {
	if maxWorkers == nil {
		return
	}
	g.mu.Lock()
	evicted := g.evictIdleLocked(keyHash, maxWorkers, nil)
	g.mu.Unlock()
	g.closeEvicted(evicted)
}

// Close stops background work. Running turns end with their processes.
func (g *Gateway) Close() { close(g.stop) }

// CallTokenOf returns the CallToken of the request the session's turn
// serves now, or "" when the session has no turn.
func (g *Gateway) CallTokenOf(account, sessionID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	sess := g.sessions[sessionRef(account, sessionID)]
	if sess == nil || sess.turn == nil {
		return ""
	}
	return sess.turn.callToken
}

// SetSnapshotTTL changes how long idle sessions stay reusable.
func (g *Gateway) SetSnapshotTTL(ttl time.Duration) { g.snaps.setTTL(ttl) }

// MigrateAccountIDs rewrites legacy account names in persisted snapshots to
// their immutable account IDs on upgrade.
func (g *Gateway) MigrateAccountIDs(nameToID map[string]string) error {
	return g.snaps.migrateAccounts(nameToID)
}

// Complete runs req for the caller's API key. The channel yields
// output events and ends with exactly one Finish or Err event. An Err
// before any output means nothing was produced, so the HTTP layer may
// still choose a status code.
func (g *Gateway) Complete(ctx context.Context, caller conv.Caller, req conv.Request) <-chan conv.Event {
	out := make(chan conv.Event)
	go func() {
		defer close(out)
		w := &response{ctx: ctx, out: out}
		rec := Record{At: time.Now(), KeyHash: caller.KeyHash, KeyName: caller.Name, API: req.API, Stream: req.Stream, Model: req.Model, CallToken: rand.Text()}
		err := g.serve(ctx, caller, req, w, &rec)
		if err != nil {
			w.fail(err)
		}
		g.report(rec, w, err)
	}()
	return out
}

func (g *Gateway) report(rec Record, w *response, err *conv.Error) {
	if g.recorder == nil {
		return
	}
	rec.Duration = time.Since(rec.At)
	if !w.firstOutput.IsZero() {
		rec.TimeToFirstOutput = w.firstOutput.Sub(rec.At)
	}
	rec.Usage, rec.Finish, rec.Err = w.usage, w.finish, err
	g.recorder.Record(rec)
}

func (g *Gateway) serve(ctx context.Context, caller conv.Caller, req conv.Request, w *response, rec *Record) *conv.Error {
	t := newTranscript(req.Messages)
	rec.Anchor = inspectAnchor(t)
	if err := validate(req, t); err != nil {
		return err
	}
	if err := g.checkSystemPrompt(t); err != nil {
		return err
	}
	if err := g.checkFlaggedPrompt(t); err != nil {
		return err
	}
	g.refreshUnreported(ctx)
	if ctx.Err() != nil {
		return &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}
	}
	offered := g.offered()
	model, ok := lookupModel(offered, req.Model, req.Effort)
	if !ok {
		return &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("unknown model %q", req.Model)}
	}
	modelID := model.id
	rec.Model, rec.QuotaGroup, rec.KeyGroup = modelID, pool.QuotaGroup(modelID), pool.KeyGroup(modelID)
	if !caller.Allows(modelID) {
		return &conv.Error{Kind: conv.ErrForbidden, Message: fmt.Sprintf("this API key may not use model %q", modelID)}
	}
	if err := g.checkLimit(ctx, caller, modelID, rec.KeyGroup); err != nil {
		return err
	}

	r := &run{user: caller.KeyHash, keyName: caller.Name, maxWorkers: caller.MaxWorkers, req: req, t: t, model: model, offered: offered, exclude: map[string]bool{}, reuse: true}
	keys := g.conversationKeys(caller.KeyHash, r.choice(), t)
	g.applyConversation(r, keys)

	at, failure := g.start(ctx, r)
	if failure == nil {
		if r.moving {
			g.log.Warn("conversation switches accounts", "to", at.account.Name(), "reason", r.moveReason)
			g.conversations.put(keys, conversationState{account: at.account.ID(), moved: r.moveReason})
		}
		g.mu.Lock()
		rec.Account, rec.AccountName, rec.Slug, rec.Effort, rec.Mode, rec.ReplayReason = at.account.ID(), at.account.Name(), at.slug, at.tier, r.mode, r.replayReason
		rec.QuotaGroup = r.poolGroup(at.account)
		at.callToken = rec.CallToken
		g.mu.Unlock()
		failure = g.consume(ctx, at, r, w)
	}
	if failure != nil {
		if failure.retry {
			g.log.Warn("request failed on its account", "account", cmp.Or(failure.accountName, failure.account), "definite", failure.definite, "err", failure.err.Message)
		}
		return g.failConversation(r, keys, failure)
	}
	g.conversations.clear(keys)
	return nil
}

func (g *Gateway) finalizeFailure(r *run, failure *attemptFailure) *conv.Error {
	if failure.quotaWait > maxRetryableQuotaWait {
		if g.hasAvailableAccount(r) {
			return &conv.Error{Kind: conv.ErrUnavailable, Message: "service temporarily unavailable"}
		}
		return &conv.Error{
			Kind:       conv.ErrQuota,
			HTTPStatus: http.StatusForbidden,
			Message:    failure.err.Message,
		}
	}
	if failure.err.Kind == conv.ErrUpstream && failure.err.HTTPStatus == 0 && !g.hasAvailableAccount(r) && g.minShortCooldown(r) == 0 {
		return &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("model %q is currently unavailable", r.req.Model)}
	}
	return failure.err
}

// checkSystemPrompt refuses, while that is on, a system prompt that is
// still a harness's default: clients must send their own.
func (g *Gateway) checkSystemPrompt(t *transcript) *conv.Error {
	detector := g.harnessPrompts.Load()
	if detector == nil {
		return nil
	}
	matches := detector.Detect(t.system)
	if len(matches) == 0 {
		return nil
	}
	return &conv.Error{Kind: conv.ErrForbidden, Message: fmt.Sprintf(
		"request rejected: your system prompt matched '%s'. replace it with your own\n\n%s",
		strings.Join(sysprompt.Harnesses(matches), "', '"), sysprompt.Excerpts(t.system, matches))}
}

// limitGroup is the group whose limit covers keyGroup. Argon & Carbon
// count in Gemini's limit when the key merges them.
func limitGroup(caller conv.Caller, keyGroup string) string {
	if keyGroup == pool.GroupArgon && caller.MergeArgon {
		return pool.GroupGemini
	}
	return keyGroup
}

// spentUnder is the key's spend that counts against the limit of group
// (a result of limitGroup), given its spend by key group.
func spentUnder(caller conv.Caller, spent map[string]float64, group string) float64 {
	if group == pool.GroupGemini && caller.MergeArgon {
		return spent[pool.GroupGemini] + spent[pool.GroupArgon]
	}
	return spent[group]
}

// checkLimit refuses a key that spent its limit: the total, or the one of
// its key group (see limitGroup). A model without a price would spend
// nothing, so a limited key may not use it. A turn running when the limit
// is reached finishes; later requests, including those continuing a
// parked turn, are refused.
func (g *Gateway) checkLimit(ctx context.Context, caller conv.Caller, modelID, keyGroup string) *conv.Error {
	group := limitGroup(caller, keyGroup)
	if !caller.LimitedIn(group) {
		return nil
	}
	if !g.budget.Priced(modelID) {
		return &conv.Error{Kind: conv.ErrForbidden, Message: fmt.Sprintf(
			"this API key has a spending limit, and model %q has no configured price; ask the administrator to add its price", modelID)}
	}
	spent, err := g.budget.Spent(ctx, caller.KeyHash)
	if err != nil {
		g.log.Error("key limit check failed", "key", caller.Name, "err", err)
		return &conv.Error{Kind: conv.ErrUnavailable, Message: "could not check this API key's spending limit"}
	}
	if caller.LimitTotal != nil {
		total := 0.0
		for _, cost := range spent {
			total += cost
		}
		if total >= *caller.LimitTotal {
			return &conv.Error{Kind: conv.ErrKeyLimit, Message: fmt.Sprintf(
				"this API key spent %s of its %s limit (at API list prices)", usd(total), usd(*caller.LimitTotal))}
		}
		return nil
	}
	if limit, used := caller.Limits[group], spentUnder(caller, spent, group); used >= limit {
		return &conv.Error{Kind: conv.ErrKeyLimit, Message: fmt.Sprintf(
			"this API key spent %s of its %s limit on %s models (at API list prices)", usd(used), usd(limit), group)}
	}
	return nil
}

// Spent returns the key's total spend in USD across all quota groups.
func (g *Gateway) Spent(ctx context.Context, keyHash string) (float64, error) {
	if g.budget == nil {
		return 0, nil
	}
	spent, err := g.budget.Spent(ctx, keyHash)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, cost := range spent {
		total += cost
	}
	return total, nil
}

// usd formats dollars with cents, and sub-dollar amounts to 1/100 cent.
func usd(v float64) string {
	if v >= 1 {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}

func validate(req conv.Request, t *transcript) *conv.Error {
	if len(t.body) == 0 {
		return &conv.Error{Kind: conv.ErrInvalidRequest, Message: "messages must contain a user message"}
	}
	seen := map[string]bool{}
	for _, tool := range req.Tools {
		if tool.Name == "" || isInternalTool(tool.Name) || seen[tool.Name] {
			return &conv.Error{Kind: conv.ErrInvalidRequest, Message: fmt.Sprintf("tool name %q is empty, duplicated, or reserved", tool.Name)}
		}
		seen[tool.Name] = true
	}
	return nil
}

// poolGroup is the quota group account charges the run's model to.
func (r *run) poolGroup(account *pool.Account) string {
	return account.PoolGroup(r.model.id)
}

// run is one request's planning state across attempts.
type run struct {
	user       string
	keyName    string
	maxWorkers *int
	req        conv.Request
	t          *transcript
	model      modelChoice
	// offered is the merged catalog the model was found in.
	offered []modelEntry
	exclude map[string]bool
	// pin is the account the conversation stays on (empty = any): the one
	// its earlier requests failed on, until its failures are due.
	pin string
	// reuse allows continuing or warming a session; a conversation that
	// left its account replays fresh.
	reuse bool
	// moving is set while the conversation leaves the account it failed
	// on; moveReason says why it replays, as ReplayReason.
	moving     bool
	moveReason ReplayReason
	// mode is how the last attempt started; replayReason is set with
	// ModeReplay.
	mode         Mode
	replayReason ReplayReason
}

func (r *run) choice() string { return r.req.Model + "\x00" + r.req.Effort }

// attemptFailure ends the request's attempt. retry means the account's
// failure counts toward switching the conversation to another account;
// definite means no retry can fix it there, so the next request switches.
type attemptFailure struct {
	err         *conv.Error
	account     string
	accountName string
	retry       bool
	definite    bool
	quotaWait   time.Duration
}

// response writes one HTTP response's events and notes what it sent.
type response struct {
	ctx     context.Context
	out     chan<- conv.Event
	emitted bool

	firstOutput time.Time
	usage       *conv.Usage
	finish      conv.FinishReason
}

func (w *response) send(ev conv.Event) bool {
	select {
	case w.out <- ev:
	case <-w.ctx.Done():
		return false
	}
	w.emitted = true
	output := ev.Text != "" || ev.Reasoning != "" || ev.ToolCalls != nil
	if output && w.firstOutput.IsZero() {
		w.firstOutput = time.Now()
	}
	if ev.Usage != nil {
		w.usage = ev.Usage
	}
	if ev.Finish != "" {
		w.finish = ev.Finish
	}
	return true
}

// sendUsage sends u unless it is nil.
func (w *response) sendUsage(u *conv.Usage) bool {
	return u == nil || w.send(conv.Event{Usage: u})
}

func (w *response) fail(err *conv.Error) {
	select {
	case w.out <- conv.Event{Err: err}:
	case <-w.ctx.Done():
	}
}

// consume streams the turn into one response until the turn ends or calls
// client tools.
func (g *Gateway) consume(ctx context.Context, at *activeTurn, r *run, w *response) *attemptFailure {
	// What agy sends after a failed model call, before the response sent
	// anything, waits until a later call succeeds or the turn ends: if the
	// turn failed, another account may still serve the request.
	var held []conv.Event
	release := func() bool {
		for _, ev := range held {
			if !w.send(ev) {
				return false
			}
		}
		held = nil
		return true
	}
	send := func(ev conv.Event) bool {
		if !w.emitted && at.account.ModelFailure(at.sess.id) != nil {
			held = append(held, ev)
			return true
		}
		return release() && w.send(ev)
	}

	var text strings.Builder
	for {
		ev, ok := at.turn.Next(ctx)
		if !ok {
			break
		}
		switch ev.Kind {
		case agy.EventText:
			text.WriteString(ev.Text)
			if !send(conv.Event{Text: ev.Text}) {
				return g.abandon(at, r)
			}
		case agy.EventThought:
			if !send(conv.Event{Reasoning: ev.Text}) {
				return g.abandon(at, r)
			}
		case agy.EventNativeTool:
			line := fmt.Sprintf("\n[agy tool %s]\n", ev.Tool)
			if ev.Failed {
				line = fmt.Sprintf("\n[agy tool %s failed]\n", ev.Tool)
			}
			if !send(conv.Event{Reasoning: line}) {
				return g.abandon(at, r)
			}
		case agy.EventExternal:
			group := ev.Payload.(*callGroup)
			reply := conv.Message{Role: conv.RoleAssistant, Text: text.String(), ToolCalls: group.toolCalls()}
			usage := g.partUsage(at)
			g.park(at, group)
			mark := g.record(at, r, &reply, 0)
			// The model call that made the tool calls succeeded.
			if !release() || !w.send(conv.Event{ToolCalls: reply.ToolCalls}) || !w.sendUsage(usage) || !w.send(conv.Event{Anchor: mark}) || !w.send(conv.Event{Finish: conv.FinishToolCalls}) {
				g.log.Info("client left after tool calls; turn stays parked", "session", at.sess.id)
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return g.abandon(at, r)
	}
	<-at.closed
	return g.finish(at, r, w, release, &text)
}

// finish handles the settled turn: errors, a failed last model call, and
// the final reply. release sends the output held back so far.
func (g *Gateway) finish(at *activeTurn, r *run, w *response, release func() bool, text *strings.Builder) *attemptFailure {
	outcome := at.turn.Outcome()
	account, accountName := at.account.ID(), at.account.Name()
	failure := at.account.ModelFailure(at.sess.id)
	at.account.ForgetModelFailure(at.sess.id)
	g.mu.Lock()
	risky := at.risky
	g.mu.Unlock()
	if risky {
		// The session holds the refused input now.
		g.forget(account, at.sess.id)
		return g.riskControl(at, r)
	}
	if outcome.Err != nil {
		g.log.Warn("turn failed", "account", accountName, "session", at.sess.id, "err", outcome.Err)
		g.forget(account, at.sess.id)
		if agy.IsAuthError(outcome.Err) {
			at.account.Disable(outcome.Err)
		}
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: "upstream request failed"}, account: account, accountName: accountName, retry: true, definite: agy.IsAuthError(outcome.Err)}
	}
	if outcome.StopReason == agy.StopCancelled {
		msg := "request cancelled by a newer request for this conversation"
		g.mu.Lock()
		if at.cancelledByAdmin {
			msg = "turn cancelled by the administrator"
		}
		g.mu.Unlock()
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: msg}}
	}

	// agy ends a turn whose model call failed for good with end_turn and
	// a line about the error as the reply.
	if failure != nil {
		// The session holds the failed input now. Its positions that
		// cannot be rewound to are gone (launch drops them); the others
		// stay, so a retry rewinds past the failed input and keeps the
		// reasoning.
		return g.modelFailure(at, r, failure)
	}

	if !release() {
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}}
	}
	mark := g.record(at, r, &conv.Message{Role: conv.RoleAssistant, Text: text.String()}, at.endSteps)
	if outcome.Usage != nil {
		rest := minusUsage(usageFrom(outcome.Usage), at.reported)
		if !w.send(conv.Event{Usage: &rest}) {
			return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}}
		}
	}
	if !w.send(conv.Event{Anchor: mark}) {
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}}
	}
	finish := conv.FinishStop
	if outcome.StopReason == agy.StopMaxTokens {
		finish = conv.FinishLength
	}
	if !w.send(conv.Event{Finish: finish}) {
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}}
	}
	return nil
}

// abandon cancels a turn whose client left mid-response. agy keeps what
// it recorded, so the request's own transcript stays reusable.
func (g *Gateway) abandon(at *activeTurn, r *run) *attemptFailure {
	g.mu.Lock()
	at.consuming = false
	g.mu.Unlock()
	at.turn.Cancel()
	g.record(at, r, nil, 0)
	return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}}
}

// park leaves the turn waiting for the client's tool results.
func (g *Gateway) park(at *activeTurn, group *callGroup) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ttl := time.Duration(g.parkTTL.Load())
	at.consuming = false
	at.parked, at.parkedAt = group, time.Now()
	at.account.EndRun(at)
	at.parkTimer = time.AfterFunc(ttl, func() {
		g.mu.Lock()
		due := at.parkedAt.Add(time.Duration(g.parkTTL.Load()))
		expired := at.parked == group && !at.finished && !time.Now().Before(due)
		g.mu.Unlock()
		if expired {
			g.log.Info("client never returned tool results; cancelling turn", "session", at.sess.id)
			at.turn.Cancel()
		}
	})
}

// usageFrom maps agy's usage to OpenAI's, whose prompt tokens include the
// cached ones and completion tokens the reasoning ones. agy's inputTokens
// excludes cache reads: a repeated 36.6k prompt reported input 28.9k +
// cachedRead 36.7k. cachedWriteTokens has only been seen as 0, so whether
// input includes it is unknown.
func usageFrom(u *agy.Usage) conv.Usage {
	return conv.Usage{
		PromptTokens:     u.InputTokens + u.CachedReadTokens,
		CompletionTokens: u.OutputTokens,
		ReasoningTokens:  u.ThoughtTokens,
		CachedTokens:     u.CachedReadTokens,
	}
}

func plusUsage(a, b conv.Usage) conv.Usage {
	return conv.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		CachedTokens:     a.CachedTokens + b.CachedTokens,
	}
}

func minusUsage(a, b conv.Usage) conv.Usage {
	return conv.Usage{
		PromptTokens:     a.PromptTokens - b.PromptTokens,
		CompletionTokens: a.CompletionTokens - b.CompletionTokens,
		ReasoningTokens:  a.ReasoningTokens - b.ReasoningTokens,
		CachedTokens:     a.CachedTokens - b.CachedTokens,
	}
}

// partUsage returns the tokens of the turn's model calls since its last
// response that reported some, for a response that ends on client tool
// calls. Each response then counts its own calls, as OpenAI's do, and
// clients see the context size instead of the whole agent loop's sum. It
// returns nil when the language server cannot tell; the turn's last
// response, which reports the turn's total less what was reported, then
// counts these calls.
func (g *Gateway) partUsage(at *activeTurn) *conv.Usage {
	g.mu.Lock()
	proc := at.sess.proc
	g.mu.Unlock()
	sum, steps, err := proc.TurnUsage(context.Background(), at.sess.id, at.usageSteps)
	if err != nil {
		g.log.Warn("usage of the turn's model calls unavailable; the turn's last response counts them", "session", at.sess.id, "err", err)
		return nil
	}
	u := usageFrom(&sum)
	at.usageSteps, at.reported = steps, plusUsage(at.reported, u)
	return &u
}

// record stores the session's latest position: the request's transcript
// plus the reply agy produced for it (nil when the reply is not reusable),
// with steps native steps (0 when it cannot be rewound to). It returns the
// anchor naming that position.
func (g *Gateway) record(at *activeTurn, r *run, reply *conv.Message, steps int) string {
	count, hash := len(r.t.body), r.t.hashes[len(r.t.body)]
	if reply != nil {
		count++
		hash = chainHashes(hash, []string{messageKey(*reply)})[1]
	}
	snap := snapshot{
		User:      r.user,
		Account:   at.account.ID(),
		SessionID: at.sess.id,
		ToolsKey:  toolsKey(r.req.Tools),
		Count:     count,
		Hash:      hash,
		Steps:     steps,
	}
	if err := g.snaps.put(snap); err != nil {
		g.log.Warn("snapshot not persisted", "err", err)
	}
	return anchor{Account: snap.Account, SessionID: snap.SessionID, Count: count, Hash: hash}.encode()
}

// movedPast drops the session's positions it cannot be rewound to, before
// it takes input past them.
func (g *Gateway) movedPast(account, sessionID string) {
	if err := g.snaps.movedPast(account, sessionID); err != nil {
		g.log.Warn("snapshot removal not persisted", "err", err)
	}
}

// forget drops the session's snapshot: its native state no longer matches
// a transcript the client holds.
func (g *Gateway) forget(account, sessionID string) {
	if err := g.snaps.drop(account, sessionID); err != nil {
		g.log.Warn("snapshot removal not persisted", "err", err)
	}
}

// watch cleans up after the turn settles: parked calls fail, the slot is
// freed, and an unobserved ending invalidates the snapshot unless the
// gateway cancelled the turn itself.
func (g *Gateway) watch(at *activeTurn) {
	<-at.turn.Done()
	outcome := at.turn.Outcome()
	if outcome.Err == nil && outcome.StopReason == agy.StopEndTurn && at.account.ModelFailure(at.sess.id) == nil {
		at.endSteps = g.countSteps(at)
	}
	g.mu.Lock()
	at.finished = true
	if at.parkTimer != nil {
		at.parkTimer.Stop()
	}
	at.account.EndRun(at)
	for _, group := range at.groups {
		group.settle(bridge.Result{}, errTurnEnded)
	}
	unobserved := !at.consuming
	if at.sess.turn == at {
		at.sess.turn = nil
	}
	at.sess.lastUsed = time.Now()
	g.releaseTurnLocked(at)
	g.mu.Unlock()

	if unobserved && outcome.StopReason != agy.StopCancelled {
		g.forget(at.account.ID(), at.sess.id)
	}
	close(at.closed)
}

// countSteps counts the session's native steps before another turn can
// claim it; 0 leaves the position unrewindable.
func (g *Gateway) countSteps(at *activeTurn) int {
	g.mu.Lock()
	proc := at.sess.proc
	g.mu.Unlock()
	steps, err := proc.StepCount(context.Background(), at.sess.id, at.baseSteps)
	if err != nil {
		g.log.Warn("native step count unavailable; position cannot be rewound to", "session", at.sess.id, "err", err)
		return 0
	}
	return steps
}

// contextOverflow matches the model errors seen for a conversation over
// the window (agy 1.5.3):
//
//	Gemini:       The input token count exceeds the maximum number of tokens allowed (1048576).
//	gpt-oss-120b: Requested token count exceeds the model's maximum context length of 131072 tokens. …
//	gpt-oss-120b: The input (183641 tokens) is longer than the model's context length (131072 tokens).
var contextOverflow = regexp.MustCompile(`exceeds the maximum number of tokens allowed|exceeds the model's maximum context length|is longer than the model's context length`)

const statusSiteOverloaded = 529

// modelFailure maps the failed model call that ended the turn. Messages
// pass through: clients recognize overflow by their text as well. A 429
// that names a retry time is the account's quota: the account cools down
// until then and the conversation moves to another account. agy retries a
// 429 without one itself before it gives up.
//
// Failures that belong to the account count toward the conversation's
// switch (see failConversation); definite ones (quota, sign-in, a model
// the account lacks) switch it with the next request.
func (g *Gateway) modelFailure(at *activeTurn, r *run, failure *pool.ModelFailure) *attemptFailure {
	account, accountName := at.account.ID(), at.account.Name()
	counted := func(err *conv.Error) *attemptFailure {
		return &attemptFailure{err: err, account: account, accountName: accountName, retry: true}
	}
	definite := func(err *conv.Error, quotaWait time.Duration) *attemptFailure {
		return &attemptFailure{err: err, account: account, accountName: accountName, retry: true, definite: true, quotaWait: quotaWait}
	}
	rejected := func(err *conv.Error) *attemptFailure {
		return &attemptFailure{err: err, account: account, accountName: accountName}
	}
	switch {
	case failure.Code == http.StatusBadRequest && contextOverflow.MatchString(failure.Message):
		return rejected(&conv.Error{Kind: conv.ErrContextLength, Message: failure.Message})
	case failure.Code == http.StatusRequestEntityTooLarge:
		return rejected(&conv.Error{Kind: conv.ErrContextLength, Message: failure.Message, HTTPStatus: failure.Code})
	case failure.Code == http.StatusBadRequest:
		return rejected(&conv.Error{Kind: conv.ErrInvalidRequest, Message: failure.Message})
	case failure.Code == http.StatusUnauthorized:
		// Upstream account token failed: never pass 401 downstream where it
		// means the caller's API key is invalid.
		return definite(&conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("model %q is currently unavailable", r.req.Model)}, 0)
	case failure.Code == http.StatusForbidden:
		if at.account.Available(r.poolGroup(at.account)) != nil {
			// VALIDATION_REQUIRED (or similar) removed this account; avoid
			// leaking account-verification details to callers.
			return definite(&conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("model %q is currently unavailable", r.req.Model)}, 0)
		}
		return definite(&conv.Error{Kind: conv.ErrForbidden, Message: failure.Message}, 0)
	case failure.Code == http.StatusNotFound:
		// Map 404 to 400 model_not_found so clients like OpenCode do not retry
		// it, when no other account can take the conversation.
		return definite(&conv.Error{Kind: conv.ErrUnknownModel, Message: failure.Message}, 0)
	case failure.Code == http.StatusTooManyRequests:
		wait := time.Until(failure.Reset)
		if wait <= 0 {
			return counted(&conv.Error{Kind: conv.ErrOverloaded, Message: failure.Message})
		}
		at.account.Cool(r.poolGroup(at.account), failure.Reset)
		return definite(&conv.Error{Kind: conv.ErrQuota, Message: failure.Message, RetryAfterSeconds: int(math.Ceil(wait.Seconds()))}, wait)
	case failure.Code == http.StatusServiceUnavailable || failure.Code == statusSiteOverloaded:
		return counted(&conv.Error{Kind: conv.ErrOverloaded, Message: failure.Message})
	case failure.Code == http.StatusRequestTimeout || failure.Code == http.StatusConflict || failure.Code >= http.StatusInternalServerError:
		return counted(&conv.Error{Kind: conv.ErrUpstream, Message: failure.Message, HTTPStatus: failure.Code})
	case failure.Code >= http.StatusBadRequest && failure.Code < http.StatusInternalServerError:
		return rejected(&conv.Error{Kind: conv.ErrInvalidRequest, Message: failure.Message, HTTPStatus: failure.Code})
	}
	return rejected(&conv.Error{Kind: conv.ErrUpstream, Message: failure.Message})
}

// janitor closes sessions idle past IdleClose (or the snapshot TTL, if
// shorter), kills agy processes that held no session for as long, drops
// sessions agy no longer holds, and now and then deletes agy's files of
// sessions nobody can continue.
func (g *Gateway) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	var pruned time.Time
	for {
		select {
		case <-g.stop:
			return
		case <-ticker.C:
		}
		g.sweep()
		if time.Since(pruned) >= conversationPruneEvery {
			g.pruneConversations()
			pruned = time.Now()
		}
	}
}

// sweep is one janitor pass.
func (g *Gateway) sweep() {
	limit := min(time.Duration(g.idleClose.Load()), g.snaps.ttl())
	var idle []*activeTurn
	var lasts []*session
	var lastUses []time.Time // lasts' lastUsed
	g.mu.Lock()
	idle = append(idle, g.evictIdleLocked("", nil, nil)...)
	byAccount := map[*pool.Account][]*session{}
	for _, sess := range g.sessions {
		if sess.turn == nil && (!sess.proc.Alive() || !sess.proc.HasSession(sess.id)) {
			g.dropSessionLocked(sess)
			continue
		}
		byAccount[sess.account] = append(byAccount[sess.account], sess)
	}
	for _, sessions := range byAccount {
		closing, last := g.idleSessionsLocked(sessions, limit)
		if last != nil {
			lasts, lastUses = append(lasts, last), append(lastUses, last.lastUsed)
		}
		for _, sess := range closing {
			idle = append(idle, g.claimClosingLocked(sess))
		}
	}
	g.mu.Unlock()

	// Asked without g.mu: keepForQuota samples the account.
	for i, sess := range lasts {
		lastUsed := lastUses[i]
		if g.keepForQuota(context.Background(), sess.account, lastUsed) {
			continue
		}
		g.mu.Lock()
		unchanged := g.sessions[sess.ref] == sess && sess.turn == nil && sess.lastUsed.Equal(lastUsed)
		if unchanged {
			idle = append(idle, g.claimClosingLocked(sess))
		}
		g.mu.Unlock()
	}
	for _, claim := range idle {
		if err := g.closeClaimed(context.Background(), claim); err != nil {
			g.log.Warn("closing an idle session failed", "session", claim.sess.id, "err", err)
			continue
		}
		g.log.Debug("idle session closed", "account", claim.sess.account.Name(), "session", claim.sess.id)
	}

	// After the closes, so a process whose last session just closed gets a
	// full idle period.
	for _, account := range g.pool.Accounts() {
		account.RetireIdleProcs(limit)
	}
}

// idleSessionsLocked picks the account's sessions idle past limit to
// close. With KeepForQuota set and all of them idle, the newest is left
// out as last, for the caller to ask about. It needs g.mu.
func (g *Gateway) idleSessionsLocked(sessions []*session, limit time.Duration) (closing []*session, last *session) {
	for _, sess := range sessions {
		if sess.turn == nil && time.Since(sess.lastUsed) > limit {
			closing = append(closing, sess)
		}
	}
	allClose := len(closing) > 0 && len(closing) == len(sessions)
	if g.keepForQuota == nil || !allClose {
		return closing, nil
	}
	newest := 0
	for i, sess := range closing {
		if sess.lastUsed.After(closing[newest].lastUsed) {
			newest = i
		}
	}
	last = closing[newest]
	return slices.Delete(closing, newest, newest+1), last
}

// claimClosingLocked reserves an idle session for closing; requests for
// it wait until the close is done and then reopen it. It needs g.mu.
func (g *Gateway) claimClosingLocked(sess *session) *activeTurn {
	claim := newActiveTurn(sess)
	claim.closing = true
	sess.turn = claim
	return claim
}

// closeClaimed closes the claimed session in agy and forgets it here;
// its snapshot stays.
func (g *Gateway) closeClaimed(ctx context.Context, claim *activeTurn) error {
	sess := claim.sess
	err := sess.proc.CloseSession(ctx, sess.id)
	g.mu.Lock()
	g.dropSessionLocked(sess)
	g.mu.Unlock()
	close(claim.closed)
	return err
}

// dropSessionLocked forgets a session agy no longer runs: closed, its
// process gone, or never reopened. agy makes no more model calls for it,
// so the failure its account keeps for it goes too: a turn the client
// left after a failed or cancelled call never read it. It needs g.mu.
func (g *Gateway) dropSessionLocked(sess *session) {
	if sess.binding != nil { // nil when reopening failed
		sess.binding.Close()
	}
	delete(g.sessions, sess.ref)
	sess.account.ForgetModelFailure(sess.id)
}

var errSessionBusy = errors.New("session is starting a turn for another request")

// errPositionGone means the session moved past the planned position while
// the request claimed it; the request plans again.
var errPositionGone = errors.New("session moved past the planned position")

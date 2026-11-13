// Package gateway turns completion requests into agy turns. ACP sessions
// are a cache of client transcripts: a request continues a turn parked on
// a client tool, reuses a session whose snapshot is a prefix of the
// request (warm), or replays the transcript into a new session (fresh).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
	"agyrelay/internal/tokens"
)

const (
	// parkTTL cancels a turn whose client never returns its tool results.
	parkTTL = 30 * time.Minute
	// defaultQuotaCooldown applies when a quota notice names no reset time.
	defaultQuotaCooldown = time.Hour
	maxAttempts          = 3
	janitorInterval      = 30 * time.Second
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
	// Recorder, when set, receives a record of every request.
	Recorder Recorder
	// Budget measures key spend; it must be set when keys have limits.
	Budget Budget
	// Refresh, when set, samples an account's quota, starting agy, which
	// reports the account's model catalog (see refreshUnreported).
	Refresh func(ctx context.Context, account *pool.Account) error
	// ContextCheck turns on the check of prompts against ContextWindows.
	ContextCheck   bool
	ContextWindows []ContextWindow
	Log            *slog.Logger
}

// Budget reports API keys' list-price spend.
type Budget interface {
	// Spent is the key's spend in USD by quota group.
	Spent(ctx context.Context, keyHash string) (map[string]float64, error)
	// Priced reports whether the model has a price, which spend needs.
	Priced(modelID string) bool
}

type Gateway struct {
	log      *slog.Logger
	pool     *pool.Pool
	bridge   *bridge.Server
	snaps    *snapshotStore
	recorder Recorder
	budget   Budget
	refresh  func(ctx context.Context, account *pool.Account) error
	// refreshes holds, by account name, a running refresh or the last one
	// that left the account without a catalog.
	refreshMu sync.Mutex
	refreshes map[string]*refreshCall
	stop      chan struct{}
	// idleClose is a time.Duration.
	idleClose atomic.Int64
	// limits is the context-window check, nil while it is off; limitsMu
	// serializes its changes.
	limits   atomic.Pointer[contextLimits]
	limitsMu sync.Mutex

	// mu guards sessions and every session's and activeTurn's mutable state.
	mu       sync.Mutex
	sessions map[string]*session // sessionRef → session
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
	consuming bool
	finished  bool
	// baseSteps is a lower bound of the session's native steps at launch.
	baseSteps int
	// endSteps is the native step count after the turn ended normally, 0
	// when unknown. It is set before closed is closed.
	endSteps int
	// closed is closed after the settled turn was cleaned up.
	closed chan struct{}
	// closing marks a claim held while the idle session is closed in agy;
	// closed is closed when that is done.
	closing bool
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
		log:       cfg.Log,
		pool:      cfg.Pool,
		bridge:    cfg.Bridge,
		snaps:     snaps,
		recorder:  cfg.Recorder,
		budget:    cfg.Budget,
		refresh:   cfg.Refresh,
		refreshes: map[string]*refreshCall{},
		stop:      make(chan struct{}),
		sessions:  map[string]*session{},
	}
	g.idleClose.Store(int64(cfg.IdleClose))
	if err := g.SetContextWindows(cfg.ContextWindows, cfg.ContextCheck); err != nil {
		return nil, err
	}
	go g.janitor()
	return g, nil
}

// SetIdleClose changes how long an idle session stays open in agy.
func (g *Gateway) SetIdleClose(d time.Duration) { g.idleClose.Store(int64(d)) }

// SetContextWindows replaces the context windows and turns their check on
// or off; models without a window are left to upstream's limit. Turning
// the check on loads the tokenizers (about 0.2 s and 55 MiB of heap),
// turning it off drops them.
func (g *Gateway) SetContextWindows(windows []ContextWindow, check bool) error {
	g.limitsMu.Lock()
	defer g.limitsMu.Unlock()
	if !check {
		if g.limits.Swap(nil) != nil {
			// The check is turned off to save memory; hand the tokenizers'
			// heap back to the OS now instead of over minutes.
			debug.FreeOSMemory()
		}
		return nil
	}
	var prompts *promptEstimator
	if current := g.limits.Load(); current != nil {
		prompts = current.prompts
	} else {
		set, err := tokens.Load()
		if err != nil {
			return fmt.Errorf("load tokenizers: %w", err)
		}
		prompts = newPromptEstimator(set)
	}
	g.limits.Store(&contextLimits{windows: windowMap(windows), prompts: prompts})
	return nil
}

// Close stops background work. Running turns end with their processes.
func (g *Gateway) Close() { close(g.stop) }

// SetSnapshotTTL changes how long idle sessions stay reusable.
func (g *Gateway) SetSnapshotTTL(ttl time.Duration) { g.snaps.setTTL(ttl) }

// Complete runs req for the caller's API key. The channel yields
// output events and ends with exactly one Finish or Err event. An Err
// before any output means nothing was produced, so the HTTP layer may
// still choose a status code.
func (g *Gateway) Complete(ctx context.Context, caller conv.Caller, req conv.Request) <-chan conv.Event {
	out := make(chan conv.Event)
	go func() {
		defer close(out)
		w := &response{ctx: ctx, out: out}
		rec := Record{At: time.Now(), KeyHash: caller.KeyHash, KeyName: caller.Name, API: req.API, Stream: req.Stream, Model: req.Model}
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
	if err := validate(req, t); err != nil {
		return err
	}
	g.refreshUnreported(ctx)
	offered := g.offered()
	model, ok := lookupModel(offered, req.Model, req.Effort)
	if !ok {
		return &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("unknown model %q", req.Model)}
	}
	modelID := model.id
	rec.Model, rec.QuotaGroup = modelID, pool.QuotaGroup(modelID)
	if !caller.Allows(modelID) {
		return &conv.Error{Kind: conv.ErrForbidden, Message: fmt.Sprintf("this API key may not use model %q", modelID)}
	}
	if err := g.checkLimit(ctx, caller, modelID, rec.QuotaGroup); err != nil {
		return err
	}
	estimate, err := g.checkContextWindow(req, t, modelID)
	if err != nil {
		return err
	}

	r := &run{user: caller.KeyHash, req: req, t: t, model: model, offered: offered, group: rec.QuotaGroup, exclude: map[string]bool{}, reuse: true}
	attempts := min(maxAttempts, len(g.pool.Accounts()))
	for attempt := 1; ; attempt++ {
		at, failure := g.start(ctx, r)
		if failure == nil {
			g.mu.Lock()
			rec.Account, rec.Slug, rec.Effort, rec.Mode, rec.ReplayReason = at.account.Name(), at.slug, at.tier, r.mode, r.replayReason
			g.mu.Unlock()
			failure = g.consume(ctx, at, r, w)
		}
		if failure == nil {
			if w.usage != nil && estimate > 0 {
				// A turn of one model call reports its prompt; compare.
				g.log.Debug("prompt estimate vs usage", "model", modelID, "mode", r.mode, "estimate", estimate, "prompt_tokens", w.usage.PromptTokens)
			}
			return nil
		}
		if !failure.retry || w.emitted || attempt >= attempts || ctx.Err() != nil {
			return failure.err
		}
		g.log.Warn("retrying on another account", "account", failure.account, "err", failure.err.Message)
		r.exclude[failure.account] = true
		r.reuse = false
	}
}

// checkLimit refuses a key that spent its limit: the total, or the quota
// group's. A model without a price would spend nothing, so a limited key
// may not use it. A turn running when the limit is reached finishes;
// later requests, including those continuing a parked turn, are refused.
func (g *Gateway) checkLimit(ctx context.Context, caller conv.Caller, modelID, group string) *conv.Error {
	if !caller.LimitedIn(group) {
		return nil
	}
	if !g.budget.Priced(modelID) {
		return &conv.Error{Kind: conv.ErrForbidden, Message: fmt.Sprintf(
			"this API key has a spending limit, and model %q has no price in agyrelay; ask the agyrelay admin to add its price (Settings → API list prices)", modelID)}
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
	if limit := caller.Limits[group]; spent[group] >= limit {
		return &conv.Error{Kind: conv.ErrKeyLimit, Message: fmt.Sprintf(
			"this API key spent %s of its %s limit on %s models (at API list prices)", usd(spent[group]), usd(limit), group)}
	}
	return nil
}

// usd formats dollars with cents, and sub-dollar amounts to 1/100 cent.
func usd(v float64) string {
	if v >= 1 {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}

// checkContextWindow refuses a request whose estimated prompt exceeds the
// model's context window. The message matches the overflow patterns of
// OpenAI clients (opencode, pi), which then compact the conversation.
// It returns the estimate, 0 when the check is off or the model has no
// window.
func (g *Gateway) checkContextWindow(req conv.Request, t *transcript, modelID string) (int, *conv.Error) {
	limits := g.limits.Load()
	if limits == nil {
		return 0, nil
	}
	window, limited := limits.windows[modelID]
	if !limited {
		return 0, nil
	}
	estimate, err := limits.prompts.estimate(modelID, req, t)
	if err != nil {
		g.log.Error("prompt token count failed", "model", modelID, "err", err)
		return 0, &conv.Error{Kind: conv.ErrUnavailable, Message: "could not count the prompt's tokens: " + err.Error()}
	}
	if estimate <= window {
		return estimate, nil
	}
	return estimate, &conv.Error{Kind: conv.ErrContextLength, Message: fmt.Sprintf(
		"This model's maximum context length is %d tokens. However, your messages resulted in about %d tokens (agyrelay's estimate). Please reduce the length of the messages.",
		window, estimate)}
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

// run is one request's planning state across attempts.
type run struct {
	user  string
	req   conv.Request
	t     *transcript
	model modelChoice
	// offered is the merged catalog the model was found in.
	offered []modelEntry
	group   string
	exclude map[string]bool
	// reuse allows continuing or warming a session; a retry replays fresh.
	reuse bool
	// mode is how the last attempt started; replayReason is set with
	// ModeReplay.
	mode         Mode
	replayReason ReplayReason
}

func (r *run) choice() string { return r.req.Model + "\x00" + r.req.Effort }

// attemptFailure ends one attempt. retry means nothing reached the client
// and another account may succeed.
type attemptFailure struct {
	err     *conv.Error
	account string
	retry   bool
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

func (w *response) fail(err *conv.Error) {
	select {
	case w.out <- conv.Event{Err: err}:
	case <-w.ctx.Done():
	}
}

// consume streams the turn into one response until the turn ends or calls
// client tools.
func (g *Gateway) consume(ctx context.Context, at *activeTurn, r *run, w *response) *attemptFailure {
	var guard noticeGuard
	var held []conv.Event // reasoning held while the guard holds text
	var text strings.Builder
	releaseHeld := func() bool {
		for _, h := range held {
			if !w.send(h) {
				return false
			}
		}
		held = nil
		return true
	}
	emit := func(ev conv.Event) bool {
		if guard.holding() {
			held = append(held, ev)
			return true
		}
		if !releaseHeld() {
			return false
		}
		if ev.Text != "" {
			text.WriteString(ev.Text)
		}
		return w.send(ev)
	}
	emitText := func(delta string) bool {
		if delta == "" {
			return true
		}
		return emit(conv.Event{Text: delta})
	}

	for {
		ev, ok := at.turn.Next(ctx)
		if !ok {
			break
		}
		switch ev.Kind {
		case agy.EventText:
			if !emitText(guard.push(ev.Text)) {
				return g.abandon(at, r)
			}
		case agy.EventThought:
			if !emit(conv.Event{Reasoning: ev.Text}) {
				return g.abandon(at, r)
			}
		case agy.EventNativeTool:
			line := fmt.Sprintf("\n[agy tool %s]\n", ev.Tool)
			if ev.Failed {
				line = fmt.Sprintf("\n[agy tool %s failed]\n", ev.Tool)
			}
			if !emit(conv.Event{Reasoning: line}) {
				return g.abandon(at, r)
			}
		case agy.EventExternal:
			// A notice never precedes a tool call: held text is a reply.
			rest := guard.flush()
			if !releaseHeld() || !emitText(rest) {
				return g.abandon(at, r)
			}
			group := ev.Payload.(*callGroup)
			reply := conv.Message{Role: conv.RoleAssistant, Text: text.String(), ToolCalls: group.toolCalls()}
			g.park(at, group)
			mark := g.record(at, r, &reply, 0)
			if !w.send(conv.Event{ToolCalls: reply.ToolCalls}) || !w.send(conv.Event{Anchor: mark}) || !w.send(conv.Event{Finish: conv.FinishToolCalls}) {
				g.log.Info("client left after tool calls; turn stays parked", "session", at.sess.id)
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return g.abandon(at, r)
	}
	<-at.turn.Done()
	return g.finish(at, r, w, &guard, held, &text)
}

// finish handles the settled turn: errors, notices, and the final reply.
func (g *Gateway) finish(at *activeTurn, r *run, w *response, guard *noticeGuard, held []conv.Event, text *strings.Builder) *attemptFailure {
	outcome := at.turn.Outcome()
	account := at.account.Name()
	if outcome.Err != nil {
		g.forget(at.account.Name(), at.sess.id)
		if agy.IsAuthError(outcome.Err) {
			at.account.Disable(outcome.Err)
		}
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: outcome.Err.Error()}, account: account, retry: true}
	}
	if outcome.StopReason == agy.StopCancelled {
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: "the Antigravity turn was cancelled by a newer request for this conversation"}}
	}

	reply := guard.flush()
	switch kind, reset, message := classifyNotice(reply, time.Now()); kind {
	case noticeQuota:
		g.forget(at.account.Name(), at.sess.id)
		if reset.IsZero() {
			reset = time.Now().Add(defaultQuotaCooldown)
		}
		at.account.Cool(r.group, reset)
		retryAfter := int(time.Until(reset).Seconds())
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrQuota, Message: message, RetryAfterSeconds: retryAfter}, account: account, retry: true}
	case noticeCapacity:
		g.forget(at.account.Name(), at.sess.id)
		return &attemptFailure{err: &conv.Error{Kind: conv.ErrOverloaded, Message: message}, account: account}
	case noticeAgentError:
		// The session holds the failed input now.
		g.forget(at.account.Name(), at.sess.id)
		return &attemptFailure{err: g.agentError(at), account: account}
	}

	for _, ev := range held {
		if !w.send(ev) {
			return nil
		}
	}
	if reply != "" {
		text.WriteString(reply)
		if !w.send(conv.Event{Text: reply}) {
			return nil
		}
	}
	<-at.closed // endSteps is set
	mark := g.record(at, r, &conv.Message{Role: conv.RoleAssistant, Text: text.String()}, at.endSteps)
	if outcome.Usage != nil {
		if !w.send(conv.Event{Usage: usageFrom(outcome.Usage)}) {
			return nil
		}
	}
	if !w.send(conv.Event{Anchor: mark}) {
		return nil
	}
	finish := conv.FinishStop
	if outcome.StopReason == agy.StopMaxTokens {
		finish = conv.FinishLength
	}
	w.send(conv.Event{Finish: finish})
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
	return &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: "client disconnected"}}
}

// park leaves the turn waiting for the client's tool results.
func (g *Gateway) park(at *activeTurn, group *callGroup) {
	g.mu.Lock()
	defer g.mu.Unlock()
	at.consuming = false
	at.parked, at.parkedAt = group, time.Now()
	at.parkTimer = time.AfterFunc(parkTTL, func() {
		g.mu.Lock()
		expired := at.parked == group && !at.finished
		g.mu.Unlock()
		if expired {
			g.log.Info("client never returned tool results; cancelling turn", "session", at.sess.id)
			at.turn.Cancel()
		}
	})
}

// usageFrom maps agy's usage to OpenAI's, whose prompt tokens include the
// cached ones. agy's inputTokens excludes cache reads: a repeated 36.6k
// prompt reported input 28.9k + cachedRead 36.7k. cachedWriteTokens has
// only been seen as 0, so whether input includes it is unknown.
func usageFrom(u *agy.Usage) *conv.Usage {
	return &conv.Usage{
		PromptTokens:     u.InputTokens + u.CachedReadTokens,
		CompletionTokens: u.OutputTokens + u.ThoughtTokens,
		ReasoningTokens:  u.ThoughtTokens,
		CachedTokens:     u.CachedReadTokens,
	}
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
		Account:   at.account.Name(),
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
	g.mu.Lock()
	at.finished = true
	if at.parkTimer != nil {
		at.parkTimer.Stop()
	}
	for _, group := range at.groups {
		group.settle(bridge.Result{}, errTurnEnded)
	}
	unobserved := !at.consuming
	if at.sess.turn == at {
		at.sess.turn = nil
	}
	at.sess.lastUsed = time.Now()
	g.mu.Unlock()

	at.account.Release()
	if unobserved && outcome.StopReason != agy.StopCancelled {
		g.forget(at.account.Name(), at.sess.id)
	}
	if outcome.Err == nil && outcome.StopReason == agy.StopEndTurn {
		at.endSteps = g.countSteps(at)
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

// agentError maps the model error that ended the turn. Messages pass
// through: clients recognize overflow by their text as well.
func (g *Gateway) agentError(at *activeTurn) *conv.Error {
	g.mu.Lock()
	proc := at.sess.proc
	g.mu.Unlock()
	cause, err := proc.LastError(context.Background(), at.sess.id, at.baseSteps)
	if err != nil {
		g.log.Warn("cause of agy's turn error unavailable", "session", at.sess.id, "err", err)
		return &conv.Error{Kind: conv.ErrUpstream, Message: agentErrorNotice + " (cause unavailable: " + err.Error() + ")"}
	}
	if cause.Code == http.StatusBadRequest && contextOverflow.MatchString(cause.Message) {
		return &conv.Error{Kind: conv.ErrContextLength, Message: cause.Message}
	}
	return &conv.Error{Kind: conv.ErrUpstream, Message: cause.Message}
}

// janitor closes sessions idle past IdleClose (or the snapshot TTL, if
// shorter), drops sessions agy no longer holds, and now and then deletes
// agy's files of sessions nobody can continue.
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
	g.mu.Lock()
	for ref, sess := range g.sessions {
		if sess.turn != nil {
			continue
		}
		if !sess.proc.Alive() || !sess.proc.HasSession(sess.id) {
			sess.binding.Close()
			delete(g.sessions, ref)
			continue
		}
		if time.Since(sess.lastUsed) > limit {
			idle = append(idle, g.claimClosingLocked(sess))
		}
	}
	g.mu.Unlock()
	for _, claim := range idle {
		if err := g.closeClaimed(context.Background(), claim); err != nil {
			g.log.Warn("closing an idle session failed", "session", claim.sess.id, "err", err)
			continue
		}
		g.log.Debug("idle session closed", "account", claim.sess.account.Name(), "session", claim.sess.id)
	}
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
	sess.binding.Close()
	delete(g.sessions, sess.ref)
	g.mu.Unlock()
	close(claim.closed)
	return err
}

var errSessionBusy = errors.New("session is starting a turn for another request")

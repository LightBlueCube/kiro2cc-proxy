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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

const (
	// parkTTL cancels a turn whose client never returns its tool results.
	parkTTL = 30 * time.Minute
	// defaultQuotaCooldown applies when a quota notice names no reset time.
	defaultQuotaCooldown = time.Hour
	maxAttempts          = 3
	janitorInterval      = 10 * time.Minute
)

type Config struct {
	Pool   *pool.Pool
	Bridge *bridge.Server
	// DataDir holds snapshots.json.
	DataDir string
	// SnapshotTTL bounds how long an idle session stays reusable.
	SnapshotTTL time.Duration
	Log         *slog.Logger
}

type Gateway struct {
	log    *slog.Logger
	pool   *pool.Pool
	bridge *bridge.Server
	snaps  *snapshotStore
	models *catalog
	ttl    time.Duration
	stop   chan struct{}

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
	// choice is the model and effort as the client asked for them; the
	// resolved slug may change once the live catalog arrives.
	choice string
	// turn is nil while the claim is being prepared.
	turn   *agy.Turn
	groups map[string]*callGroup
	// parked is the group the last response ended on.
	parked    *callGroup
	parkTimer *time.Timer
	// consuming is true while a response reads the turn's events.
	consuming bool
	finished  bool
	// closed is closed after the settled turn was cleaned up.
	closed chan struct{}
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
		log:      cfg.Log,
		pool:     cfg.Pool,
		bridge:   cfg.Bridge,
		snaps:    snaps,
		models:   newCatalog(),
		ttl:      cfg.SnapshotTTL,
		stop:     make(chan struct{}),
		sessions: map[string]*session{},
	}
	go g.janitor()
	return g, nil
}

// Close stops background work. Running turns end with their processes.
func (g *Gateway) Close() { close(g.stop) }

func (g *Gateway) Models() []conv.ModelInfo { return g.models.list() }

// Complete runs req for user (the API key's name). The channel yields
// output events and ends with exactly one Finish or Err event. An Err
// before any output means nothing was produced, so the HTTP layer may
// still choose a status code.
func (g *Gateway) Complete(ctx context.Context, user string, req conv.Request) <-chan conv.Event {
	out := make(chan conv.Event)
	go func() {
		defer close(out)
		w := &response{ctx: ctx, out: out}
		if err := g.serve(ctx, user, req, w); err != nil {
			w.fail(err)
		}
	}()
	return out
}

func (g *Gateway) serve(ctx context.Context, user string, req conv.Request, w *response) *conv.Error {
	t := newTranscript(req.Messages)
	if err := validate(req, t); err != nil {
		return err
	}
	slug, ok := g.models.resolve(req.Model, req.Effort)
	if !ok {
		return &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("unknown model %q", req.Model)}
	}

	r := &run{user: user, req: req, t: t, slug: slug, group: pool.QuotaGroup(slug), exclude: map[string]bool{}, reuse: true}
	attempts := min(maxAttempts, len(g.pool.Accounts()))
	for attempt := 1; ; attempt++ {
		at, failure := g.start(ctx, r)
		if failure == nil {
			failure = g.consume(ctx, at, r, w)
		}
		if failure == nil {
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
	user    string
	req     conv.Request
	t       *transcript
	slug    string
	group   string
	exclude map[string]bool
	// reuse allows continuing or warming a session; a retry replays fresh.
	reuse bool
}

func (r *run) choice() string { return r.req.Model + "\x00" + r.req.Effort }

// attemptFailure ends one attempt. retry means nothing reached the client
// and another account may succeed.
type attemptFailure struct {
	err     *conv.Error
	account string
	retry   bool
}

// response writes one HTTP response's events.
type response struct {
	ctx     context.Context
	out     chan<- conv.Event
	emitted bool
}

func (w *response) send(ev conv.Event) bool {
	select {
	case w.out <- ev:
		w.emitted = true
		return true
	case <-w.ctx.Done():
		return false
	}
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
			mark := g.record(at, r, &reply)
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
	mark := g.record(at, r, &conv.Message{Role: conv.RoleAssistant, Text: text.String()})
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
	g.record(at, r, nil)
	return &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: "client disconnected"}}
}

// park leaves the turn waiting for the client's tool results.
func (g *Gateway) park(at *activeTurn, group *callGroup) {
	g.mu.Lock()
	defer g.mu.Unlock()
	at.consuming = false
	at.parked = group
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

func usageFrom(u *agy.Usage) *conv.Usage {
	return &conv.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens + u.ThoughtTokens,
		ReasoningTokens:  u.ThoughtTokens,
		CachedTokens:     u.CachedReadTokens,
	}
}

// record stores the session's snapshot: the request's transcript plus the
// reply agy produced for it (nil when the reply is not reusable). It
// returns the anchor naming that position.
func (g *Gateway) record(at *activeTurn, r *run, reply *conv.Message) string {
	count, hash := len(r.t.body), r.t.hashes[len(r.t.body)]
	if reply != nil {
		count++
		hash = chainHashes(hash, []string{messageKey(*reply)})[1]
	}
	snap := snapshot{
		User:       r.user,
		Account:    at.account.Name(),
		SessionID:  at.sess.id,
		ToolsKey:   toolsKey(r.req.Tools),
		SystemHash: hashString(r.t.system),
		Count:      count,
		Hash:       hash,
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
	close(at.closed)
}

// janitor evicts idle sessions past the snapshot TTL and sessions of dead
// processes.
func (g *Gateway) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ticker.C:
		}
		g.mu.Lock()
		for ref, sess := range g.sessions {
			idle := sess.turn == nil && time.Since(sess.lastUsed) > g.ttl
			dead := sess.turn == nil && !sess.proc.Alive()
			if idle || dead {
				sess.binding.Close()
				delete(g.sessions, ref)
			}
		}
		g.mu.Unlock()
	}
}

var errSessionBusy = errors.New("session is starting a turn for another request")

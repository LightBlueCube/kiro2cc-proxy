package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

// start plans the attempt and returns a running turn:
//
// The snapshot comes from the request's anchors, else from the user's own
// snapshots.
//
//	snapshot prefix hit ─┬─ parked turn answered by the delta ──▶ continue
//	                     └─ else (cancel what runs there, rewind
//	                        to a rewindable position) ──────────▶ warm delta
//	no hit, or warm failed ─────────────────────────────────────▶ fresh replay
//
// Input past a position that cannot be rewound to removes it, so a
// cancelled turn's input is never sent again on top of itself: the next
// request plans from an earlier rewindable position.
func (g *Gateway) start(ctx context.Context, r *run) (*activeTurn, *attemptFailure) {
	if !r.reuse {
		return g.startFresh(ctx, r, ReplayRetry)
	}
	for {
		snap, ok := g.anchoredSnapshot(r.t)
		if !ok {
			snap, ok = g.snaps.lookup(r.user, r.t)
		}
		if !ok {
			return g.startFresh(ctx, r, ReplayNoSnapshot)
		}
		if snap.ToolsKey != toolsKey(r.req.Tools) {
			return g.startFresh(ctx, r, ReplayToolsChanged)
		}
		if at := g.tryContinue(snap, r); at != nil {
			r.mode = ModeContinue
			return at, nil
		}
		at, err := g.startWarm(ctx, snap, r)
		if err == nil {
			return at, nil
		}
		if errors.Is(err, errPositionGone) {
			continue
		}
		if ctx.Err() != nil {
			return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: ctx.Err().Error()}}
		}
		g.log.Info("warm session unusable; replaying fresh", "session", snap.SessionID, "err", err)
		return g.startFresh(ctx, r, ReplayWarmFailed)
	}
}

// tryContinue hands the client's tool results to a parked turn whose
// boundary snapshot is the request's prefix.
func (g *Gateway) tryContinue(snap snapshot, r *run) *activeTurn {
	g.mu.Lock()
	sess := g.sessions[sessionRef(snap.Account, snap.SessionID)]
	if sess == nil || sess.turn == nil {
		g.mu.Unlock()
		return nil
	}
	at := sess.turn
	sameTurn := at.parked != nil && !at.finished && at.choice == r.choice()
	if !sameTurn {
		g.mu.Unlock()
		return nil
	}
	results, trailing, ok := matchResults(at.parked, r.t.body[snap.Count:])
	if !ok {
		g.mu.Unlock()
		return nil
	}
	group := at.parked
	at.parked = nil
	at.parkTimer.Stop()
	at.consuming = true
	g.mu.Unlock()

	g.movedPast(snap.Account, snap.SessionID)
	group.settle(groupResult(group, results, trailing), nil)
	return at
}

// startWarm prompts the snapshot's session with the transcript delta.
func (g *Gateway) startWarm(ctx context.Context, snap snapshot, r *run) (*activeTurn, error) {
	account := g.pool.Account(snap.Account)
	if account == nil {
		return nil, fmt.Errorf("account %s is no longer configured", snap.Account)
	}
	if err := account.Available(r.group); err != nil {
		return nil, err
	}
	slug, tier, ok := slugOn(account, r)
	if !ok {
		return nil, fmt.Errorf("account %s does not offer model %s", snap.Account, r.req.Model)
	}
	at, err := g.claimSession(ctx, snap, account, r)
	if err != nil {
		if !errors.Is(err, errSessionBusy) && ctx.Err() == nil {
			g.forget(snap.Account, snap.SessionID)
		}
		return nil, err
	}
	// Claiming may have waited for a cancelled turn that took input past
	// snap.
	if _, ok := g.snaps.position(snap.Account, snap.SessionID, snap.Count, snap.Hash); !ok {
		g.releaseClaim(at)
		return nil, errPositionGone
	}
	r.mode = ModeWarm
	if snap.Steps > 0 {
		rewound, err := g.rewindTo(ctx, at, snap, r)
		if err != nil {
			g.releaseClaim(at)
			if ctx.Err() == nil {
				g.forget(snap.Account, snap.SessionID)
			}
			return nil, err
		}
		if rewound {
			r.mode = ModeRewind
		}
	}
	if err := account.TryAcquire(r.group); err != nil {
		g.releaseClaim(at)
		return nil, err
	}
	g.launch(at, account, r, slug, tier, deltaPromptBlocks(r.t.body[snap.Count:]), snap.Steps)
	return at, nil
}

// rewindTo returns the claimed session to snap's position when turns ran
// past it (later replies, or what a cancelled turn left), and reports
// whether it did.
func (g *Gateway) rewindTo(ctx context.Context, at *activeTurn, snap snapshot, r *run) (bool, error) {
	g.mu.Lock()
	proc, binding := at.sess.proc, at.sess.binding
	g.mu.Unlock()
	steps, err := proc.StepCount(ctx, snap.SessionID, snap.Steps)
	if err != nil {
		return false, err
	}
	if steps == snap.Steps {
		return false, nil
	}
	if err := proc.Rewind(ctx, snap.SessionID, snap.Steps, sessionSpec(at.sess.account, binding, r)); err != nil {
		return false, err
	}
	g.log.Info("rewound session", "session", snap.SessionID, "steps", steps, "kept", snap.Steps)
	if err := g.snaps.rewound(snap.Account, snap.SessionID, snap.Count); err != nil {
		g.log.Warn("snapshot rewind not persisted", "err", err)
	}
	return true, nil
}

// claimSession reserves the snapshot's session for a new turn, cancelling
// the turn that runs there, and reopens the session when this process no
// longer holds it.
func (g *Gateway) claimSession(ctx context.Context, snap snapshot, account *pool.Account, r *run) (*activeTurn, error) {
	ref := sessionRef(snap.Account, snap.SessionID)
	for {
		g.mu.Lock()
		sess := g.sessions[ref]
		if sess != nil && sess.turn != nil {
			old := sess.turn
			g.mu.Unlock()
			if old.closing {
				// Being closed for idleness: wait, then reopen it.
				select {
				case <-old.closed:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if old.turn == nil {
				return nil, errSessionBusy
			}
			old.turn.Cancel()
			select {
			case <-old.closed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		live := sess != nil && sess.proc.Alive() && sess.proc.HasSession(snap.SessionID)
		if sess == nil {
			sess = &session{ref: ref, id: snap.SessionID, user: snap.User, account: account}
			g.sessions[ref] = sess
		}
		at := newActiveTurn(sess)
		sess.turn = at
		g.mu.Unlock()
		if live {
			return at, nil
		}
		if err := g.reopen(ctx, sess, r); err != nil {
			g.mu.Lock()
			if sess.binding != nil {
				sess.binding.Close()
			}
			delete(g.sessions, ref)
			g.mu.Unlock()
			return nil, err
		}
		return at, nil
	}
}

// reopen restores a persisted session into the account's current process.
func (g *Gateway) reopen(ctx context.Context, sess *session, r *run) error {
	if _, err := os.Stat(sess.account.ConversationStore(sess.id)); err != nil {
		return fmt.Errorf("conversation store: %w", err)
	}
	tools := r.req.Tools
	proc, err := sess.account.Proc(context.WithoutCancel(ctx), profileFor(tools))
	if err != nil {
		return err
	}
	binding, err := g.bridge.Bind(bridgeTools(tools), sessionTools{g: g, sess: sess})
	if err != nil {
		return err
	}
	if err := proc.ReopenSession(ctx, sess.id, sessionSpec(sess.account, binding, r)); err != nil {
		binding.Close()
		return err
	}
	g.mu.Lock()
	if sess.binding != nil {
		sess.binding.Close()
	}
	sess.proc, sess.binding, sess.known = proc, binding, toolNames(tools)
	g.mu.Unlock()
	return nil
}

// startFresh opens a new session on the best account that offers the
// model and replays the transcript; reason says why, when it has history.
// Accounts that fail to open one are skipped.
func (g *Gateway) startFresh(ctx context.Context, r *run, reason ReplayReason) (*activeTurn, *attemptFailure) {
	var lastErr error
	var lacking []string
	for {
		account, err := g.pool.Pick(r.user, r.group, r.exclude)
		if err != nil {
			// Catalogs that sessions just reported may show no account
			// has the model, as the fallback list had promised.
			if _, offered := lookupModel(g.offered(), r.req.Model, r.req.Effort); !offered {
				return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("unknown model %q", r.req.Model)}}
			}
			message := err.Error()
			if len(lacking) > 0 {
				message += fmt.Sprintf("; model %s is not offered by %s", r.req.Model, strings.Join(lacking, ", "))
			}
			if lastErr != nil {
				message += "; last error: " + lastErr.Error()
			}
			return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUnavailable, Message: message}}
		}
		skip := func() {
			account.Release()
			r.exclude[account.Name()] = true
			lacking = append(lacking, account.Name())
		}
		if _, _, ok := slugOn(account, r); !ok {
			skip()
			continue
		}
		at, err := g.openSession(ctx, r, account)
		if err == nil {
			// The new session reported the account's catalog, so this is
			// exact even for an account that had not reported one; the
			// unused session is closed when idle.
			slug, tier, ok := slugOn(account, r)
			if !ok {
				g.releaseClaim(at)
				skip()
				continue
			}
			r.mode, r.replayReason = ModeFresh, ""
			if newestInput(r.t.body) > 0 {
				r.mode, r.replayReason = ModeReplay, reason
			}
			g.launch(at, account, r, slug, tier, freshPrompt(r.t), 0)
			return at, nil
		}
		account.Release()
		if ctx.Err() != nil {
			return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: ctx.Err().Error()}}
		}
		if agy.IsAuthError(err) {
			account.Disable(err)
		}
		g.log.Warn("account could not open a session", "account", account.Name(), "err", err)
		r.exclude[account.Name()] = true
		lastErr = err
	}
}

// openSession creates a session on account and claims it.
func (g *Gateway) openSession(ctx context.Context, r *run, account *pool.Account) (*activeTurn, error) {
	proc, err := account.Proc(context.WithoutCancel(ctx), profileFor(r.req.Tools))
	if err != nil {
		return nil, err
	}
	sess := &session{user: r.user, account: account, proc: proc, known: toolNames(r.req.Tools)}
	binding, err := g.bridge.Bind(bridgeTools(r.req.Tools), sessionTools{g: g, sess: sess})
	if err != nil {
		return nil, err
	}
	id, models, err := proc.NewSession(ctx, sessionSpec(account, binding, r))
	if err != nil {
		binding.Close()
		return nil, err
	}
	account.SetModels(models)
	sess.id, sess.ref, sess.binding = id, sessionRef(account.Name(), id), binding
	at := newActiveTurn(sess)
	g.mu.Lock()
	sess.turn = at
	g.sessions[sess.ref] = sess
	g.mu.Unlock()
	return at, nil
}

// launch starts the prompt with the model slug (effort tier) on a claimed
// session that has at least baseSteps native steps. The slot on account is
// held until the turn settles.
func (g *Gateway) launch(at *activeTurn, account *pool.Account, r *run, slug, tier string, prompt []agy.ContentBlock, baseSteps int) {
	g.movedPast(account.Name(), at.sess.id)
	g.mu.Lock()
	at.account, at.choice, at.slug, at.tier, at.consuming, at.baseSteps, at.started = account, r.choice(), slug, tier, true, baseSteps, time.Now()
	at.usageSteps = baseSteps
	at.turn = at.sess.proc.RunTurn(agy.TurnSpec{SessionID: at.sess.id, Model: slug, Prompt: prompt})
	at.sess.lastUsed = time.Now()
	g.mu.Unlock()
	go g.watch(at)
}

// releaseClaim gives up a claim that never launched.
func (g *Gateway) releaseClaim(at *activeTurn) {
	g.mu.Lock()
	if at.sess.turn == at {
		at.sess.turn = nil
	}
	g.mu.Unlock()
}

// sessionSpec appends the tool policy and the client's leading system
// messages, verbatim, to agy's system prompt. A session always gets the
// same text, since the system prompt seeds its prefix hashes: resuming a
// live session with a different append adds it next to the old one
// (verified on 1.5.3).
func sessionSpec(account *pool.Account, binding *bridge.Binding, r *run) agy.SessionSpec {
	appended := toolPolicy(r.req.Tools)
	if r.t.system != "" {
		appended += "\n\n" + r.t.system
	}
	return agy.SessionSpec{
		Cwd: account.Workspace(),
		MCPServers: []agy.MCPServer{{
			Name:    agy.BridgeServer,
			Type:    "http",
			URL:     binding.URL(),
			Headers: []agy.HTTPHeader{{Name: bridge.TokenHeader, Value: binding.Token()}},
		}},
		SystemAppend: appended,
	}
}

func toolNames(tools []conv.ToolSpec) map[string]bool {
	known := make(map[string]bool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = true
	}
	return known
}
